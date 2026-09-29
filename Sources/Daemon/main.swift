import Foundation

// jacad: the Jaca background daemon and its command-line client.
//
//   jacad serve [--idle SECONDS]        run the daemon (normally started on demand)
//   jacad call METHOD [PARAMS_JSON]     one request; prints the result JSON
//   jacad watch TOPIC...                subscribe; prints each event as a JSON line
//   jacad status | stop | describe      shorthands for daemon.status, daemon.shutdown, api.describe
//
// Client commands start the daemon if it isn't running, unless --no-spawn is given.
// See docs/daemon-plan.md.

setvbuf(stdout, nil, _IOLBF, 0)

func fail(_ message: String, code: Int32 = 1) -> Never {
    FileHandle.standardError.write(Data("jacad: \(message)\n".utf8))
    exit(code)
}

let usage = """
usage: jacad serve [--idle SECONDS]
       jacad call METHOD [PARAMS_JSON] [--no-spawn]
       jacad watch TOPIC... [--no-spawn]
       jacad status | stop | describe [--no-spawn]
"""

var args = Array(CommandLine.arguments.dropFirst())
let noSpawn = args.contains("--no-spawn")
args.removeAll { $0 == "--no-spawn" }
guard let command = args.first else { fail(usage, code: 2) }
args.removeFirst()

/// Signal sources, held for the life of the process.
var signalSources: [DispatchSourceSignal] = []

/// Runs `body` on the main actor and keeps the main run loop alive for it. Client commands
/// exit when `body` returns; `serve` never returns from it.
func runMain(_ body: @escaping @MainActor () async -> Int32) -> Never {
    Task { @MainActor in exit(await body()) }
    RunLoop.main.run()
    exit(0)
}

func connect() async -> DaemonClient {
    do {
        let client = try await DaemonLauncher.connect(spawn: !noSpawn)
        let _: DaemonServer.HelloResult = try await client.call(
            "hello", DaemonServer.HelloParams(protocolVersion: DaemonProtocol.version, client: "jacad-cli"))
        return client
    } catch {
        fail(error.localizedDescription)
    }
}

/// Prints a response line's `result` as compact JSON, or its error to stderr (returns 1).
func printResult(_ line: Data) -> Int32 {
    guard let object = try? JSONSerialization.jsonObject(with: line) as? [String: Any] else {
        fail("unreadable response")
    }
    if let error = object["error"] as? [String: Any] {
        let message = error["message"] as? String ?? "error"
        FileHandle.standardError.write(Data("jacad: \(message)\n".utf8))
        return 1
    }
    let result = object["result"] ?? NSNull()
    if let data = try? JSONSerialization.data(withJSONObject: result, options: [.fragmentsAllowed, .sortedKeys, .withoutEscapingSlashes]),
       let text = String(data: data, encoding: .utf8) {
        print(text)
    }
    return 0
}

func call(_ method: String, _ paramsText: String?) async -> Int32 {
    var params: Data?
    if let paramsText {
        let data = Data(paramsText.utf8)
        guard (try? JSONSerialization.jsonObject(with: data, options: [.fragmentsAllowed])) != nil else {
            fail("PARAMS_JSON is not valid JSON")
        }
        params = data
    }
    let client = await connect()
    do {
        // callRaw returns error responses as thrown RPCErrors; print those like any other failure.
        return printResult(try await client.callRaw(method, paramsJSON: params))
    } catch {
        fail(error.localizedDescription)
    }
}

switch command {
case "serve":
    var idle: TimeInterval = 300
    if let i = args.firstIndex(of: "--idle"), i + 1 < args.count, let v = TimeInterval(args[i + 1]) { idle = v }
    if let v = ProcessInfo.processInfo.environment["JACAD_IDLE_SECONDS"].flatMap(TimeInterval.init) { idle = v }
    // Leave the spawning app's session so quitting (or killing) it doesn't take the daemon along.
    setsid()
    signal(SIGPIPE, SIG_IGN)
    let server = DaemonServer(paths: .default, idleTimeout: idle)
    runMain {
        server.onStop = { exit(0) }
        do {
            try server.start()
        } catch DaemonServer.StartError.alreadyRunning {
            DaemonLog.info("another jacad holds the lock; exiting")
            return 0
        } catch {
            DaemonLog.error(error.localizedDescription)
            return 1
        }
        DaemonAreas.install(on: server)
        for sig in [SIGTERM, SIGINT] {
            signal(sig, SIG_IGN)
            let source = DispatchSource.makeSignalSource(signal: sig, queue: .main)
            source.setEventHandler { server.stop() }
            source.resume()
            signalSources.append(source)
        }
        // Serve until stopped; `onStop` exits the process.
        while true { try? await Task.sleep(for: .seconds(3600)) }
    }

case "call":
    guard let method = args.first else { fail(usage, code: 2) }
    let params = args.count > 1 ? args[1] : nil
    runMain { await call(method, params) }

case "status":
    runMain { await call("daemon.status", nil) }

case "describe":
    runMain { await call("api.describe", nil) }

case "stop":
    runMain {
        guard let client = try? await DaemonClient.connect() else { return 0 }   // not running
        return printResult((try? await client.callRaw("daemon.shutdown", paramsJSON: nil)) ?? Data("{}".utf8))
    }

case "watch":
    guard !args.isEmpty else { fail(usage, code: 2) }
    let topics = args
    runMain {
        let client = await connect()
        do {
            for await event in try await client.subscribe(topics) {
                if let text = String(data: event.line, encoding: .utf8) {
                    print(text.trimmingCharacters(in: .newlines))
                }
            }
            fail("connection closed")
        } catch {
            fail(error.localizedDescription)
        }
    }

default:
    fail(usage, code: 2)
}
