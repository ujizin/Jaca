import CoreImage
import CoreImage.CIFilterBuiltins

/// Generates a QR code as PNG bytes from a string (CoreImage, no dependency). PNG rather than
/// an `NSImage` so it can cross the daemon socket and be drawn by a non-AppKit client.
enum QRCode {
    static func png(_ string: String, scale: CGFloat = 10) -> Data? {
        let filter = CIFilter.qrCodeGenerator()
        filter.message = Data(string.utf8)
        filter.correctionLevel = "M"
        guard let output = filter.outputImage?.transformed(by: CGAffineTransform(scaleX: scale, y: scale)),
              let space = CGColorSpace(name: CGColorSpace.sRGB) else {
            return nil
        }
        return CIContext().pngRepresentation(of: output, format: .RGBA8, colorSpace: space)
    }
}

/// This Mac's LAN IPv4 address (for the QR URL the phone connects back to).
enum LANAddress {
    static func current() -> String? {
        var address: String?
        var ifaddr: UnsafeMutablePointer<ifaddrs>?
        guard getifaddrs(&ifaddr) == 0, let first = ifaddr else { return nil }
        defer { freeifaddrs(ifaddr) }
        for ptr in sequence(first: first, next: { $0.pointee.ifa_next }) {
            let flags = Int32(ptr.pointee.ifa_flags)
            let addr = ptr.pointee.ifa_addr.pointee
            // up, not loopback, IPv4
            guard (flags & (IFF_UP | IFF_LOOPBACK)) == IFF_UP, addr.sa_family == UInt8(AF_INET) else { continue }
            let name = String(cString: ptr.pointee.ifa_name)
            guard name == "en0" || name == "en1" else { continue }   // Wi-Fi / Ethernet
            var host = [CChar](repeating: 0, count: Int(NI_MAXHOST))
            if getnameinfo(ptr.pointee.ifa_addr, socklen_t(addr.sa_len), &host, socklen_t(host.count),
                           nil, 0, NI_NUMERICHOST) == 0 {
                address = String(cString: host)
                break
            }
        }
        return address
    }
}
