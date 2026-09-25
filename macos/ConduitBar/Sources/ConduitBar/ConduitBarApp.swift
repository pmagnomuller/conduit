#if os(macOS)
import AppKit
import ConduitCore
import SwiftUI

@main
struct ConduitBarApp: App {
    @State private var model: AppModel

    init() {
        let model = AppModel()
        _model = State(initialValue: model)
        model.start()
    }

    var body: some Scene {
        MenuBarExtra {
            MenuPanel(model: model)
                .onAppear { model.panelOpened() }
                .onDisappear { model.panelClosed() }
        } label: {
            MenuLabel(title: model.barTitle, alert: model.alert)
        }
        .menuBarExtraStyle(.window)

        Settings {
            SettingsView(store: model.settings)
        }
    }
}

struct MenuLabel: View {
    var title: String
    var alert: Bool

    var body: some View {
        // MenuBarExtra templates SwiftUI images, which drops a tint. A non-template
        // NSImage keeps the red mark when the gateway is down or a breaker is open.
        HStack(spacing: 4) {
            Image(nsImage: icon)
            Text(title)
        }
    }

    private var icon: NSImage {
        let symbol = alert ? "exclamationmark.circle.fill" : "point.3.connected.trianglepath.dotted"
        let base = NSImage(systemSymbolName: symbol, accessibilityDescription: nil) ?? NSImage()
        let palette: [NSColor] = alert ? [.systemRed] : [.labelColor]
        let config = NSImage.SymbolConfiguration(pointSize: 13, weight: .medium)
            .applying(.init(paletteColors: palette))
        let image = base.withSymbolConfiguration(config) ?? base
        image.isTemplate = false
        return image
    }
}

struct MenuPanel: View {
    @Bindable var model: AppModel

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                header
                if !model.reachable {
                    Text(model.banner.isEmpty ? "gateway not running" : model.banner)
                        .font(.callout.weight(.semibold))
                        .foregroundStyle(.red)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
                modeRow
                if model.mode == "pinned" || model.pinIntent {
                    pinSection
                }
                if model.reachable {
                    lastRequest
                    breakerSection
                    countsSection
                    if showDecisions { decisionsSection }
                }
                if !model.actionError.isEmpty {
                    Text(model.actionError)
                        .font(.caption)
                        .foregroundStyle(.red)
                }
                controls
            }
            .padding(12)
            .frame(width: 340, alignment: .leading)
        }
        .frame(maxHeight: 560)
    }

    private var header: some View {
        HStack {
            Text("conduit")
                .font(.headline)
            Spacer()
            Text(model.mode)
                .font(.caption.weight(.semibold))
                .padding(.horizontal, 8)
                .padding(.vertical, 2)
                .background(modeTint.opacity(0.15), in: Capsule())
                .foregroundStyle(modeTint)
        }
    }

    private var modeTint: Color {
        switch model.mode {
        case "pinned": return .orange
        case "jev": return .accentColor
        default: return .secondary
        }
    }

    private var modeRow: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Routing mode")
                .font(.caption.weight(.semibold))
                .foregroundStyle(.secondary)
            HStack(spacing: 0) {
                modeButton("Auto", value: "auto", enabled: true, help: "anthropic; breaker open → \(failover)")
                modeButton("Pinned", value: "pinned", enabled: true, help: "all traffic to one provider")
                modeButton(
                    "Jev",
                    value: "jev",
                    enabled: model.jevEnabled,
                    help: model.jevEnabled ? "ask Jev per request" : "set TYPESAFE_API_KEY in ~/.config/conduit/.env"
                )
            }
            .background(.quaternary.opacity(0.35), in: RoundedRectangle(cornerRadius: 8))
            Text(modeHelp)
                .font(.caption)
                .foregroundStyle(.secondary)
        }
    }

    private var failover: String {
        let name = model.status?.failoverProvider ?? ""
        return name.isEmpty ? "glm" : name
    }

    private var modeHelp: String {
        switch model.mode {
        case "pinned":
            let provider = model.route?.forcedProvider ?? ""
            let forcedModel = model.route?.forcedModel ?? ""
            if provider.isEmpty { return "pick a provider below" }
            return forcedModel.isEmpty ? "all traffic → \(provider)" : "all traffic → \(provider) / \(forcedModel)"
        case "jev":
            return "per request: Jev picks provider+model; breaker still wins"
        default:
            return "anthropic; breaker open → \(failover)"
        }
    }

    private func modeButton(_ title: String, value: String, enabled: Bool, help: String) -> some View {
        let selected = model.mode == value || (value == "pinned" && model.pinIntent)
        return Button {
            guard enabled else { return }
            Task { await model.selectMode(value) }
        } label: {
            Text(title)
                .font(.callout.weight(selected ? .semibold : .regular))
                .frame(maxWidth: .infinity)
                .padding(.vertical, 6)
                .background(selected ? Color.accentColor : Color.clear, in: RoundedRectangle(cornerRadius: 7))
                .foregroundStyle(selected ? Color.white : Color.primary)
                .opacity(enabled ? 1 : 0.45)
        }
        .buttonStyle(.plain)
        .help(help)
    }

    private var pinSection: some View {
        PinSection(model: model)
    }

    private var lastRequest: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("Last request")
                .font(.caption.weight(.semibold))
                .foregroundStyle(.secondary)
            if let last = model.status?.lastRequest ?? model.route?.lastRequest, !last.provider.isEmpty {
                let when = last.at.map { relativeTime(from: $0, now: Date()) } ?? ""
                Text("\(last.provider) \(last.upstreamModel) \(when)")
                    .font(.callout)
            } else {
                Text("no requests yet")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }
        }
    }

    private var breakerSection: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("Breaker")
                .font(.caption.weight(.semibold))
                .foregroundStyle(.secondary)
            let entries = model.status?.breaker.entries ?? [:]
            if entries.isEmpty {
                Text("closed · all upstreams healthy")
                    .font(.callout)
                    .foregroundStyle(.green)
            } else {
                ForEach(entries.keys.sorted(), id: \.self) { key in
                    if let entry = entries[key] {
                        BreakerRow(name: key, entry: entry)
                    }
                }
            }
        }
    }

    private var countsSection: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("Counts")
                .font(.caption.weight(.semibold))
                .foregroundStyle(.secondary)
            let counts = model.status?.counts
            Text("anthropic \(counts?.anthropicRequests ?? 0) · glm \(counts?.glmRequests ?? 0) · deepseek \(counts?.deepseekRequests ?? 0)")
                .font(.callout.monospacedDigit())
            let up = counts?.startedAt.map { uptime(since: $0, now: Date()) } ?? "—"
            Text("failovers \(counts?.failovers ?? 0) · uptime \(up)")
                .font(.callout.monospacedDigit())
                .foregroundStyle(.secondary)
        }
    }

    private var showDecisions: Bool {
        let recent = model.route?.jev?.recent ?? []
        return model.mode == "jev" || !recent.isEmpty
    }

    private var decisionsSection: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("Jev decisions")
                .font(.caption.weight(.semibold))
                .foregroundStyle(.secondary)
            let recent = Array((model.route?.jev?.recent ?? []).prefix(10))
            if recent.isEmpty {
                Text("no decisions yet")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            } else {
                ForEach(Array(recent.enumerated()), id: \.offset) { _, decision in
                    DecisionRow(decision: decision)
                }
            }
        }
    }

    private var controls: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Button("Open web UI") { model.openWebUI() }
                Button("Start gateway") { Task { await model.startGateway() } }
                Button("Stop gateway") { Task { await model.stopGateway() } }
            }
            HStack {
                Button("Settings…") {
                    NSApp.activate(ignoringOtherApps: true)
                    NSApp.sendAction(Selector(("showSettingsWindow:")), to: nil, from: nil)
                }
                Spacer()
                Button("Quit") { NSApp.terminate(nil) }
            }
        }
        .font(.callout)
    }
}

private struct PinSection: View {
    var model: AppModel
    @State private var provider = ""
    @State private var modelName = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Pin")
                .font(.caption.weight(.semibold))
                .foregroundStyle(.secondary)
            Picker("Provider", selection: $provider) {
                if providers.isEmpty {
                    Text("none").tag("")
                }
                ForEach(providers, id: \.self) { name in
                    Text(name).tag(name)
                }
            }
            .labelsHidden()
            Picker("Model", selection: $modelName) {
                Text("default mapping").tag("")
                ForEach(models, id: \.self) { name in
                    Text(name).tag(name)
                }
            }
            .labelsHidden()
            .disabled(provider.isEmpty)
            HStack {
                Button("Apply") { Task { await model.pin(provider: provider, model: modelName) } }
                    .disabled(provider.isEmpty)
                Button("Clear") { Task { await model.clearPin() } }
            }
        }
        .onAppear(perform: seed)
        .onChange(of: providers) { _, _ in
            if provider.isEmpty { seed() }
        }
    }

    private var providers: [String] {
        orderedProviders(model.route?.available ?? [:])
    }

    private var models: [String] {
        model.route?.available[provider] ?? []
    }

    private func seed() {
        let current = model.route?.forcedProvider ?? ""
        provider = current.isEmpty ? (providers.first ?? "") : current
        let forced = model.route?.forcedModel ?? ""
        modelName = models.contains(forced) ? forced : ""
    }
}

private struct BreakerRow: View {
    var name: String
    var entry: BreakerEntry

    var body: some View {
        let now = Date()
        let label = breakerLabel(entry, now: now)
        let open = isTrulyOpen(entry, now: now)
        VStack(alignment: .leading, spacing: 1) {
            HStack(spacing: 6) {
                Text(label)
                    .font(.caption.weight(.semibold))
                    .foregroundStyle(open ? Color.red : (label == "expired" ? Color.orange : Color.green))
                Text(name.replacingOccurrences(of: "|", with: " "))
                    .font(.caption)
                    .lineLimit(1)
                if let until = entry.until, entry.state.uppercased() == "OPEN" {
                    Text(countdown(until: until, now: now))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
            if !entry.reason.isEmpty {
                Text(entry.reason)
                    .font(.caption2)
                    .foregroundStyle(.secondary)
                    .lineLimit(2)
            }
        }
    }
}

private struct DecisionRow: View {
    var decision: Decision

    var body: some View {
        let requested = decision.requestedModel.isEmpty ? "?" : decision.requestedModel
        let chosen = "\(decision.provider)/\(decision.model)"
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            Text("\(requested) → \(chosen)")
                .font(.caption)
                .lineLimit(1)
            Spacer(minLength: 4)
            Text(decision.source)
                .font(.caption2)
                .foregroundStyle(.secondary)
            if let confidence = decision.confidence, confidence > 0 {
                Text("\(Int((confidence * 100).rounded()))%")
                    .font(.caption2.monospacedDigit())
                    .foregroundStyle(.secondary)
            }
        }
    }
}

struct SettingsView: View {
    @Bindable var store: SettingsStore

    var body: some View {
        Form {
            TextField("Gateway URL", text: $store.baseURLString)
                .textFieldStyle(.roundedBorder)
            Toggle("Launch at login", isOn: Binding(
                get: { store.launchAtLogin },
                set: { store.setLaunchAtLogin($0) }
            ))
            if !store.launchAtLoginMessage.isEmpty {
                Text(store.launchAtLoginMessage)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Toggle("Notifications", isOn: $store.notificationsEnabled)
        }
        .padding(20)
        .frame(width: 380)
        .onAppear { store.refreshLaunchAtLogin() }
    }
}
#endif
