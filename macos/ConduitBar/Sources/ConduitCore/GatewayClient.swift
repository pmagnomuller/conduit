import Foundation
#if canImport(FoundationNetworking)
import FoundationNetworking
#endif

enum GatewayError: Error, Equatable {
    case invalidBaseURL
    case unreachable
    case rejected(status: Int, message: String)
}

struct GatewayClient: Sendable {
    var session: URLSession

    static func makeDefault() -> GatewayClient {
        let config = URLSessionConfiguration.ephemeral
        config.timeoutIntervalForRequest = 3
        config.timeoutIntervalForResource = 4
        return GatewayClient(session: URLSession(configuration: config))
    }

    func status(base: URL) async throws -> GatewayStatus {
        try await get(base, path: "/_gateway/status")
    }

    func route(base: URL) async throws -> GatewayRoute {
        try await get(base, path: "/_gateway/route")
    }

    /// URLSession sends no Origin header. The gateway rejects a foreign Origin on this POST.
    func postRoute(base: URL, command: RouteCommand) async throws {
        let url = try endpoint(base, "/_gateway/route")
        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONEncoder().encode(command)
        let (data, response) = try await send(request)
        try check(response, data: data)
    }

    private func get<T: Decodable>(_ base: URL, path: String) async throws -> T {
        let url = try endpoint(base, path)
        let (data, response) = try await send(URLRequest(url: url))
        try check(response, data: data)
        do {
            return try JSONDecoder().decode(T.self, from: data)
        } catch {
            throw GatewayError.rejected(status: (response as? HTTPURLResponse)?.statusCode ?? 0, message: "unexpected payload")
        }
    }

    private func send(_ request: URLRequest) async throws -> (Data, URLResponse) {
        do {
            return try await session.data(for: request)
        } catch {
            throw GatewayError.unreachable
        }
    }

    private func check(_ response: URLResponse, data: Data) throws {
        guard let http = response as? HTTPURLResponse else {
            throw GatewayError.unreachable
        }
        guard (200..<300).contains(http.statusCode) else {
            throw GatewayError.rejected(status: http.statusCode, message: errorMessage(data) ?? "HTTP \(http.statusCode)")
        }
    }

    private func errorMessage(_ data: Data) -> String? {
        struct Body: Decodable { var error: String? }
        if let parsed = try? JSONDecoder().decode(Body.self, from: data),
           let message = parsed.error?.trimmingCharacters(in: .whitespacesAndNewlines),
           !message.isEmpty {
            return message
        }
        let text = String(data: data, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        return text.isEmpty ? nil : text
    }
}

func endpoint(_ base: URL, _ path: String) throws -> URL {
    var raw = base.absoluteString
    while raw.hasSuffix("/") { raw.removeLast() }
    guard let url = URL(string: raw + path) else {
        throw GatewayError.invalidBaseURL
    }
    guard url.scheme == "http" || url.scheme == "https", url.host != nil else {
        throw GatewayError.invalidBaseURL
    }
    return url
}
