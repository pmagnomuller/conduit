#if os(macOS)
import Foundation

enum LaunchAgent {
    static let label = "com.pedro.conduit"

    static func start() async -> String? {
        await run(["kickstart", "-k", "gui/\(getuid())/\(label)"])
    }

    static func stop() async -> String? {
        await run(["kill", "TERM", "gui/\(getuid())/\(label)"])
    }

    private static func run(_ args: [String]) async -> String? {
        await withCheckedContinuation { continuation in
            DispatchQueue.global(qos: .userInitiated).async {
                let process = Process()
                process.executableURL = URL(fileURLWithPath: "/bin/launchctl")
                process.arguments = args
                let pipe = Pipe()
                process.standardOutput = pipe
                process.standardError = pipe
                do {
                    try process.run()
                } catch {
                    continuation.resume(returning: error.localizedDescription)
                    return
                }
                process.waitUntilExit()
                let data = pipe.fileHandleForReading.readDataToEndOfFile()
                let text = String(data: data, encoding: .utf8)?
                    .trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
                if process.terminationStatus == 0 {
                    continuation.resume(returning: nil)
                } else if text.isEmpty {
                    continuation.resume(returning: "launchctl exited \(process.terminationStatus)")
                } else {
                    continuation.resume(returning: text)
                }
            }
        }
    }
}
#endif
