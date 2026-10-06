import Foundation
import NIOCore
import NIOPosix

/// Splits an inbound byte stream into lines (without the `\n`). A line longer than
/// `maxLength` is a protocol violation and fails the channel instead of buffering forever.
final class LineFrameDecoder: ByteToMessageDecoder {
    typealias InboundOut = ByteBuffer

    struct LineTooLong: Error {}

    private let maxLength: Int
    private var scanned = 0

    init(maxLength: Int = DaemonProtocol.maxLineBytes) { self.maxLength = maxLength }

    func decode(context: ChannelHandlerContext, buffer: inout ByteBuffer) throws -> DecodingState {
        let view = buffer.readableBytesView
        let start = view.startIndex + scanned
        if let nl = view[start...].firstIndex(of: 0x0A) {
            let length = nl - view.startIndex
            var line = buffer.readSlice(length: length)!
            buffer.moveReaderIndex(forwardBy: 1)
            scanned = 0
            // Tolerate CRLF from hand-typed clients.
            if line.readableBytesView.last == 0x0D { line = line.getSlice(at: line.readerIndex, length: line.readableBytes - 1)! }
            context.fireChannelRead(wrapInboundOut(line))
            return .continue
        }
        scanned = view.count
        if scanned > maxLength { throw LineTooLong() }
        return .needMoreData
    }

    func decodeLast(context: ChannelHandlerContext, buffer: inout ByteBuffer, seenEOF: Bool) throws -> DecodingState {
        // A trailing line without a newline is dropped; every message must end in `\n`.
        buffer.moveReaderIndex(to: buffer.writerIndex)
        return .needMoreData
    }
}

/// One side of a socket connection, as seen by the code that owns it (server or client).
/// Thread-safe: writes hop to the channel's event loop.
final class DaemonPeer: @unchecked Sendable {
    let id: Int
    let channel: Channel

    init(id: Int, channel: Channel) {
        self.id = id
        self.channel = channel
    }

    /// Writes one pre-encoded line (it must already end in `\n`).
    func send(_ line: Data) {
        let channel = self.channel
        channel.eventLoop.execute {
            guard channel.isActive else { return }
            var buffer = channel.allocator.buffer(capacity: line.count)
            buffer.writeBytes(line)
            channel.writeAndFlush(buffer, promise: nil)
        }
    }

    /// Whether the socket can take more bytes right now. False while the peer is slow and the
    /// outbound buffer is above the high watermark — droppable events are skipped then.
    var isWritable: Bool { channel.isWritable }

    func close() {
        channel.close(promise: nil)
    }
}

/// Hands each decoded line to a closure and reports the connection closing.
final class DaemonLineHandler: ChannelInboundHandler {
    typealias InboundIn = ByteBuffer

    private let onLine: (Data) -> Void
    private let onClose: () -> Void
    /// Closes the connection when it stays unwritable this long. Droppable events stop at the
    /// watermark, but responses and retained/ordered events don't, so a client that stopped
    /// reading its socket (a raw-socket script, a hung process) would otherwise grow the outbound
    /// buffer forever. `DaemonClient` reads eagerly, so the app and `jacad` never trip this.
    private let stallLimit: TimeAmount?
    private var stallCheck: Scheduled<Void>?

    init(onLine: @escaping (Data) -> Void,
         onClose: @escaping () -> Void,
         closeIfStalledFor stallLimit: TimeAmount? = nil) {
        self.onLine = onLine
        self.onClose = onClose
        self.stallLimit = stallLimit
    }

    func channelRead(context: ChannelHandlerContext, data: NIOAny) {
        var buffer = unwrapInboundIn(data)
        guard let bytes = buffer.readBytes(length: buffer.readableBytes), !bytes.isEmpty else { return }
        onLine(Data(bytes))
    }

    func channelInactive(context: ChannelHandlerContext) {
        onClose()
        context.fireChannelInactive()
    }

    func channelWritabilityChanged(context: ChannelHandlerContext) {
        if let stallLimit {
            stallCheck?.cancel()
            stallCheck = nil
            if !context.channel.isWritable {
                let channel = context.channel
                stallCheck = context.eventLoop.scheduleTask(in: stallLimit) {
                    if !channel.isWritable { channel.close(promise: nil) }
                }
            }
        }
        context.fireChannelWritabilityChanged()
    }

    func errorCaught(context: ChannelHandlerContext, error: Error) {
        // A malformed stream (line too long, reset by peer) ends this connection only.
        context.close(promise: nil)
    }
}

enum DaemonTransport {
    /// Outbound buffer bounds. Above `high` the peer counts as slow and droppable events are
    /// skipped; it becomes writable again below `low`.
    static let waterMark = ChannelOptions.Types.WriteBufferWaterMark(low: 1 << 20, high: 4 << 20)

    static var group: EventLoopGroup { MultiThreadedEventLoopGroup.singleton }
}
