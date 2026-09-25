import Foundation

struct NotifySignal: Equatable, Sendable {
    enum Kind: Equatable, Sendable {
        case failover(Int)
        case breakerOpened(String)
        case gatewayDown
        case gatewayUp
    }

    var kind: Kind

    var id: String {
        switch kind {
        case .failover: return "failover"
        case .breakerOpened(let key): return "breaker:\(key)"
        case .gatewayDown: return "down"
        case .gatewayUp: return "up"
        }
    }

    var body: String {
        switch kind {
        case .failover(let count):
            return "Failover count is \(count)"
        case .breakerOpened(let key):
            return "Breaker open: \(key)"
        case .gatewayDown:
            return "Gateway is not running"
        case .gatewayUp:
            return "Gateway is back"
        }
    }
}

/// First sample is a baseline. Repeats of the same id inside `debounce` are dropped
/// so a poll flicker does not notify twice.
struct NotificationTracker {
    var debounce: TimeInterval = 30
    private var primed = false
    private var failovers = 0
    private var openKeys: Set<String> = []
    private var reachable = false
    private var lastFire: [String: Date] = [:]

    mutating func observe(
        reachable: Bool,
        failovers: Int,
        openKeys: Set<String>,
        now: Date,
        enabled: Bool
    ) -> [NotifySignal] {
        defer {
            primed = true
            self.failovers = failovers
            self.openKeys = openKeys
            self.reachable = reachable
        }
        guard primed, enabled else { return [] }

        var pending: [NotifySignal] = []
        if failovers > self.failovers {
            pending.append(NotifySignal(kind: .failover(failovers)))
        }
        for key in openKeys.subtracting(self.openKeys).sorted() {
            pending.append(NotifySignal(kind: .breakerOpened(key)))
        }
        if self.reachable && !reachable {
            pending.append(NotifySignal(kind: .gatewayDown))
        } else if !self.reachable && reachable {
            pending.append(NotifySignal(kind: .gatewayUp))
        }

        return pending.filter { signal in
            if let previous = lastFire[signal.id], now.timeIntervalSince(previous) < debounce {
                return false
            }
            lastFire[signal.id] = now
            return true
        }
    }
}
