import Foundation

struct LastRequest: Decodable, Sendable, Equatable {
    var at: Date?
    var provider: String
    var upstreamModel: String

    enum CodingKeys: String, CodingKey {
        case at, provider
        case upstreamModel = "upstream_model"
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        at = c.flexDate(.at)
        provider = c.flexString(.provider)
        upstreamModel = c.flexString(.upstreamModel)
    }
}

struct BreakerEntry: Decodable, Sendable, Equatable {
    var state: String
    var until: Date?
    var reason: String
    var openedAt: Date?
    var lastChange: Date?

    enum CodingKeys: String, CodingKey {
        case state, until, reason
        case openedAt = "opened_at"
        case lastChange = "last_change"
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        state = c.flexString(.state)
        until = c.flexDate(.until)
        reason = c.flexString(.reason)
        openedAt = c.flexDate(.openedAt)
        lastChange = c.flexDate(.lastChange)
    }
}

struct LastQuota: Decodable, Sendable, Equatable {
    var at: Date?
    var model: String
    var reason: String

    enum CodingKeys: String, CodingKey {
        case at, model, reason
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        at = c.flexDate(.at)
        model = c.flexString(.model)
        reason = c.flexString(.reason)
    }
}

struct BreakerSnapshot: Decodable, Sendable, Equatable {
    var entries: [String: BreakerEntry]
    var lastQuota: LastQuota?
    var mode: String

    enum CodingKeys: String, CodingKey {
        case entries
        case lastQuota = "last_quota"
        case mode
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        entries = (try? c.decodeIfPresent([String: BreakerEntry].self, forKey: .entries)) ?? [:]
        lastQuota = try? c.decodeIfPresent(LastQuota.self, forKey: .lastQuota)
        mode = c.flexString(.mode)
    }
}

struct Counts: Decodable, Sendable, Equatable {
    var anthropicRequests: Int
    var glmRequests: Int
    var deepseekRequests: Int
    var failovers: Int
    var transientRetries: Int
    var jevDecisions: Int
    var jevFailOpen: Int
    var startedAt: Date?

    enum CodingKeys: String, CodingKey {
        case anthropicRequests = "anthropic_requests"
        case glmRequests = "glm_requests"
        case deepseekRequests = "deepseek_requests"
        case failovers
        case transientRetries = "transient_retries"
        case jevDecisions = "jev_decisions"
        case jevFailOpen = "jev_fail_open"
        case startedAt = "started_at"
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        anthropicRequests = c.flexInt(.anthropicRequests)
        glmRequests = c.flexInt(.glmRequests)
        deepseekRequests = c.flexInt(.deepseekRequests)
        failovers = c.flexInt(.failovers)
        transientRetries = c.flexInt(.transientRetries)
        jevDecisions = c.flexInt(.jevDecisions)
        jevFailOpen = c.flexInt(.jevFailOpen)
        startedAt = c.flexDate(.startedAt)
    }
}

struct GatewayStatus: Decodable, Sendable, Equatable {
    var listen: String
    var routing: String
    var mode: String
    var jevEnabled: Bool
    var failoverProvider: String
    var forcedProvider: String
    var forcedModel: String
    var lastRequest: LastRequest?
    var breaker: BreakerSnapshot
    var counts: Counts
    var upstream: [String: String]

    enum CodingKeys: String, CodingKey {
        case listen, routing, mode
        case jevEnabled = "jev_enabled"
        case failoverProvider = "failover_provider"
        case forcedProvider = "forced_provider"
        case forcedModel = "forced_model"
        case lastRequest = "last_request"
        case breaker, counts, upstream
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        listen = c.flexString(.listen)
        routing = c.flexString(.routing)
        mode = c.flexString(.mode)
        jevEnabled = c.flexBool(.jevEnabled)
        failoverProvider = c.flexString(.failoverProvider)
        forcedProvider = c.flexString(.forcedProvider)
        forcedModel = c.flexString(.forcedModel)
        lastRequest = try? c.decodeIfPresent(LastRequest.self, forKey: .lastRequest)
        breaker = (try? c.decodeIfPresent(BreakerSnapshot.self, forKey: .breaker)) ?? BreakerSnapshot.empty
        counts = (try? c.decodeIfPresent(Counts.self, forKey: .counts)) ?? Counts.empty
        upstream = (try? c.decodeIfPresent([String: String].self, forKey: .upstream)) ?? [:]
    }
}

extension BreakerSnapshot {
    static let empty = BreakerSnapshot(entries: [:], lastQuota: nil, mode: "")

    init(entries: [String: BreakerEntry], lastQuota: LastQuota?, mode: String) {
        self.entries = entries
        self.lastQuota = lastQuota
        self.mode = mode
    }
}

extension Counts {
    static let empty = Counts(
        anthropicRequests: 0, glmRequests: 0, deepseekRequests: 0,
        failovers: 0, transientRetries: 0, jevDecisions: 0, jevFailOpen: 0,
        startedAt: nil
    )

    init(
        anthropicRequests: Int, glmRequests: Int, deepseekRequests: Int,
        failovers: Int, transientRetries: Int, jevDecisions: Int, jevFailOpen: Int,
        startedAt: Date?
    ) {
        self.anthropicRequests = anthropicRequests
        self.glmRequests = glmRequests
        self.deepseekRequests = deepseekRequests
        self.failovers = failovers
        self.transientRetries = transientRetries
        self.jevDecisions = jevDecisions
        self.jevFailOpen = jevFailOpen
        self.startedAt = startedAt
    }
}

struct CatalogEntry: Decodable, Sendable, Equatable {
    var provider: String
    var model: String
    var profile: String

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        provider = c.flexString(.provider)
        model = c.flexString(.model)
        profile = c.flexString(.profile)
    }

    enum CodingKeys: String, CodingKey {
        case provider, model, profile
    }
}

struct Decision: Decodable, Sendable, Equatable {
    var at: Date?
    var requestedModel: String
    var provider: String
    var model: String
    var step: String
    var lease: String
    var source: String
    var reason: String
    var confidence: Double?
    var margin: Double?
    var pick: String
    var policy: String
    var estInputUSD: Double?
    var baselineInputUSD: Double?
    var latencyMS: Int?

    enum CodingKeys: String, CodingKey {
        case at
        case requestedModel = "requested_model"
        case provider, model, step, lease, source, reason, confidence, margin, pick, policy
        case estInputUSD = "est_input_usd"
        case baselineInputUSD = "baseline_input_usd"
        case latencyMS = "latency_ms"
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        at = c.flexDate(.at)
        requestedModel = c.flexString(.requestedModel)
        provider = c.flexString(.provider)
        model = c.flexString(.model)
        step = c.flexString(.step)
        lease = c.flexString(.lease)
        source = c.flexString(.source)
        reason = c.flexString(.reason)
        confidence = c.flexDouble(.confidence)
        margin = c.flexDouble(.margin)
        pick = c.flexString(.pick)
        policy = c.flexString(.policy)
        estInputUSD = c.flexDouble(.estInputUSD)
        baselineInputUSD = c.flexDouble(.baselineInputUSD)
        if let value = try? c.decodeIfPresent(Int.self, forKey: .latencyMS) {
            latencyMS = value
        } else if let value = c.flexDouble(.latencyMS) {
            latencyMS = Int(value)
        } else {
            latencyMS = nil
        }
    }
}

struct JevSnapshot: Decodable, Sendable, Equatable {
    var enabled: Bool
    var catalog: [CatalogEntry]
    var recent: [Decision]

    enum CodingKeys: String, CodingKey {
        case enabled, catalog, recent
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        enabled = c.flexBool(.enabled)
        catalog = (try? c.decodeIfPresent([CatalogEntry].self, forKey: .catalog)) ?? []
        recent = (try? c.decodeIfPresent([Decision].self, forKey: .recent)) ?? []
    }
}

struct GatewayRoute: Decodable, Sendable, Equatable {
    var mode: String
    var forcedProvider: String
    var forcedModel: String
    var available: [String: [String]]
    var jev: JevSnapshot?
    var lastRequest: LastRequest?

    enum CodingKeys: String, CodingKey {
        case mode
        case forcedProvider = "forced_provider"
        case forcedModel = "forced_model"
        case available, jev
        case lastRequest = "last_request"
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        mode = c.flexString(.mode)
        forcedProvider = c.flexString(.forcedProvider)
        forcedModel = c.flexString(.forcedModel)
        available = (try? c.decodeIfPresent([String: [String]].self, forKey: .available)) ?? [:]
        jev = try? c.decodeIfPresent(JevSnapshot.self, forKey: .jev)
        lastRequest = try? c.decodeIfPresent(LastRequest.self, forKey: .lastRequest)
    }
}

enum RouteCommand: Encodable, Equatable, Sendable {
    case auto
    case jev
    case pinned
    case pin(provider: String, model: String?)
    case clear

    private enum CodingKeys: String, CodingKey {
        case mode, provider, model, clear
    }

    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        switch self {
        case .auto:
            try c.encode("auto", forKey: .mode)
        case .jev:
            try c.encode("jev", forKey: .mode)
        case .pinned:
            try c.encode("pinned", forKey: .mode)
        case .pin(let provider, let model):
            try c.encode(provider, forKey: .provider)
            if let model, !model.isEmpty {
                try c.encode(model, forKey: .model)
            }
        case .clear:
            try c.encode(true, forKey: .clear)
        }
    }
}
