import Foundation

func menuBarTitle(reachable: Bool, mode: String, forcedProvider: String, lastModel: String) -> String {
    guard reachable else { return "offline" }
    switch mode {
    case "jev":
        return "jev"
    case "pinned":
        if forcedProvider.isEmpty { return "pinned" }
        return "pinned \(forcedProvider)"
    default:
        if let short = shortModel(lastModel) {
            return "auto · \(short)"
        }
        return "auto"
    }
}

/// `claude-opus-5-5` reads as `opus-5-5` in the menu bar; the provider is already in the mode.
func shortModel(_ model: String) -> String? {
    var text = model.trimmingCharacters(in: .whitespacesAndNewlines)
    guard !text.isEmpty else { return nil }
    if text.hasPrefix("claude-") {
        text = String(text.dropFirst("claude-".count))
    }
    if text.count > 22 {
        return String(text.prefix(21)) + "…"
    }
    return text
}

func isTrulyOpen(_ entry: BreakerEntry, now: Date) -> Bool {
    guard entry.state.uppercased() == "OPEN" else { return false }
    if let until = entry.until, until <= now { return false }
    return true
}

func breakerLabel(_ entry: BreakerEntry, now: Date) -> String {
    if entry.state.uppercased() == "OPEN", let until = entry.until, until <= now {
        return "expired"
    }
    if entry.state.isEmpty { return "closed" }
    return entry.state.lowercased()
}

func trulyOpenKeys(_ entries: [String: BreakerEntry], now: Date) -> Set<String> {
    Set(entries.compactMap { key, entry in isTrulyOpen(entry, now: now) ? key : nil })
}

func countdown(until: Date, now: Date) -> String {
    let secs = Int(until.timeIntervalSince(now).rounded())
    if secs <= 0 { return "expired" }
    if secs >= 90 { return "in \(Int((Double(secs) / 60).rounded()))m" }
    return "in \(secs)s"
}

func relativeTime(from date: Date, now: Date) -> String {
    let secs = Int(now.timeIntervalSince(date).rounded())
    if secs < 5 { return "just now" }
    if secs < 60 { return "\(secs)s ago" }
    if secs < 3600 { return "\(secs / 60)m ago" }
    if secs < 86_400 { return "\(secs / 3600)h ago" }
    return "\(secs / 86_400)d ago"
}

func uptime(since date: Date, now: Date) -> String {
    let secs = max(0, Int(now.timeIntervalSince(date).rounded()))
    let days = secs / 86_400
    let hours = (secs % 86_400) / 3600
    let mins = (secs % 3600) / 60
    if days > 0 { return "\(days)d \(hours)h" }
    if hours > 0 { return "\(hours)h \(mins)m" }
    return "\(mins)m"
}

func orderedProviders(_ available: [String: [String]]) -> [String] {
    let preferred = ["anthropic", "glm", "deepseek"]
    var out: [String] = []
    for name in preferred where available[name] != nil {
        out.append(name)
    }
    for name in available.keys.sorted() where !out.contains(name) {
        out.append(name)
    }
    return out
}

func resolvedMode(routeMode: String, statusMode: String, forcedProvider: String) -> String {
    for candidate in [routeMode, statusMode] {
        if candidate == "auto" || candidate == "pinned" || candidate == "jev" {
            return candidate
        }
    }
    return forcedProvider.isEmpty ? "auto" : "pinned"
}
