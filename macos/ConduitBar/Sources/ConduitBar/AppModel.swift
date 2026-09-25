#if os(macOS)
import AppKit
import ConduitCore
import Observation
import ServiceManagement

@MainActor
@Observable
final class SettingsStore {
    private enum Key {
        static let base = "conduitbar.baseURL"
        static let notes = "conduitbar.notifications"
    }

    static let defaultBase = "http://127.0.0.1:8787"

    var baseURLString: String {
        didSet { UserDefaults.standard.set(baseURLString, forKey: Key.base) }
    }

    var notificationsEnabled: Bool {
        didSet { UserDefaults.standard.set(notificationsEnabled, forKey: Key.notes) }
    }

    var launchAtLogin = false
    var launchAtLoginMessage = ""

    var baseURL: URL {
        let trimmed = baseURLString.trimmingCharacters(in: .whitespacesAndNewlines)
        return URL(string: trimmed) ?? URL(string: Self.defaultBase)!
    }

    init() {
        let defaults = UserDefaults.standard
        let stored = defaults.string(forKey: Key.base)?.trimmingCharacters(in: .whitespacesAndNewlines)
        baseURLString = (stored?.isEmpty == false) ? stored! : Self.defaultBase
        if defaults.object(forKey: Key.notes) == nil {
            notificationsEnabled = true
        } else {
            notificationsEnabled = defaults.bool(forKey: Key.notes)
        }
    }

    func refreshLaunchAtLogin() {
        switch SMAppService.mainApp.status {
        case .enabled, .requiresApproval:
            launchAtLogin = true
        case .notRegistered, .notFound:
            launchAtLogin = false
        @unknown default:
            break
        }
        if SMAppService.mainApp.status == .requiresApproval {
            launchAtLoginMessage = "macOS needs approval in System Settings"
        }
    }

    func setLaunchAtLogin(_ enabled: Bool) {
        do {
            if enabled {
                try SMAppService.mainApp.register()
            } else {
                try SMAppService.mainApp.unregister()
            }
            launchAtLoginMessage = ""
            refreshLaunchAtLogin()
        } catch {
            launchAtLoginMessage = error.localizedDescription
            refreshLaunchAtLogin()
        }
    }
}

@MainActor
@Observable
final class AppModel {
    let settings = SettingsStore()
    private let client = GatewayClient.makeDefault()
    private let notifier = Notifier()
    private var tracker = NotificationTracker()
    private var pollTask: Task<Void, Never>?
    private var sleepTask: Task<Void, Never>?

    var status: GatewayStatus?
    var route: GatewayRoute?
    var reachable = false
    var banner = ""
    var actionError = ""
    var panelVisible = false
    var pinIntent = false
    private var seenFailovers = 0
    private var seenOpen: Set<String> = []

    var mode: String {
        resolvedMode(
            routeMode: route?.mode ?? "",
            statusMode: status?.mode ?? "",
            forcedProvider: route?.forcedProvider ?? status?.forcedProvider ?? ""
        )
    }

    var jevEnabled: Bool {
        if let enabled = route?.jev?.enabled { return enabled }
        return status?.jevEnabled ?? false
    }

    var barTitle: String {
        menuBarTitle(
            reachable: reachable,
            mode: mode,
            forcedProvider: route?.forcedProvider ?? status?.forcedProvider ?? "",
            lastModel: status?.lastRequest?.upstreamModel ?? route?.lastRequest?.upstreamModel ?? ""
        )
    }

    var alert: Bool {
        if !reachable { return true }
        return !(trulyOpenKeys(status?.breaker.entries ?? [:], now: Date()).isEmpty)
    }

    func start() {
        guard pollTask == nil else { return }
        settings.refreshLaunchAtLogin()
        pollTask = Task { [weak self] in
            await self?.pollLoop()
        }
    }

    func panelOpened() {
        let wasOpen = panelVisible
        panelVisible = true
        if !wasOpen { sleepTask?.cancel() }
    }

    func panelClosed() {
        let wasOpen = panelVisible
        panelVisible = false
        if wasOpen { sleepTask?.cancel() }
    }

    func selectMode(_ next: String) async {
        actionError = ""
        switch next {
        case "auto":
            pinIntent = false
            await post(.auto)
        case "jev":
            pinIntent = false
            guard jevEnabled else { return }
            await post(.jev)
        case "pinned":
            let provider = route?.forcedProvider ?? ""
            if provider.isEmpty {
                pinIntent = true
                return
            }
            await post(.pinned)
        default:
            break
        }
    }

    func pin(provider: String, model: String) async {
        actionError = ""
        guard !provider.isEmpty else {
            actionError = "pick a provider first"
            return
        }
        await post(.pin(provider: provider, model: model))
    }

    func clearPin() async {
        actionError = ""
        pinIntent = false
        await post(.clear)
    }

    func openWebUI() {
        guard let url = webUIURL() else {
            actionError = "invalid gateway URL"
            return
        }
        NSWorkspace.shared.open(url)
    }

    func startGateway() async {
        actionError = ""
        if let message = await LaunchAgent.start() {
            actionError = message
            return
        }
        try? await Task.sleep(for: .milliseconds(500))
        await refresh()
    }

    func stopGateway() async {
        actionError = ""
        if let message = await LaunchAgent.stop() {
            actionError = message
            return
        }
        try? await Task.sleep(for: .milliseconds(300))
        await refresh()
    }

    func webUIURL() -> URL? {
        if let listen = status?.listen, !listen.isEmpty, reachable {
            return URL(string: "http://\(listen)/_gateway/ui")
        }
        return try? endpoint(settings.baseURL, "/_gateway/ui")
    }

    private func post(_ command: RouteCommand) async {
        do {
            try await client.postRoute(base: settings.baseURL, command: command)
            actionError = ""
            await refresh()
        } catch let error as GatewayError {
            actionError = message(for: error)
        } catch {
            actionError = error.localizedDescription
        }
    }

    private func pollLoop() async {
        while !Task.isCancelled {
            await refresh()
            let seconds: Double = panelVisible ? 2 : 10
            let sleep = Task { try? await Task.sleep(for: .seconds(seconds)) }
            sleepTask = sleep
            await sleep.value
        }
    }

    private func refresh() async {
        let now = Date()
        do {
            async let statusTask = client.status(base: settings.baseURL)
            async let routeTask = client.route(base: settings.baseURL)
            let (nextStatus, nextRoute) = try await (statusTask, routeTask)
            status = nextStatus
            route = nextRoute
            reachable = true
            banner = ""
            let open = trulyOpenKeys(nextStatus.breaker.entries, now: now)
            seenFailovers = nextStatus.counts.failovers
            seenOpen = open
            await emit(tracker.observe(
                reachable: true,
                failovers: nextStatus.counts.failovers,
                openKeys: open,
                now: now,
                enabled: settings.notificationsEnabled
            ))
        } catch let error as GatewayError {
            reachable = false
            banner = error == .invalidBaseURL ? "invalid gateway URL" : "gateway not running"
            await emit(tracker.observe(
                reachable: false,
                failovers: seenFailovers,
                openKeys: seenOpen,
                now: now,
                enabled: settings.notificationsEnabled
            ))
        } catch {
            reachable = false
            banner = "gateway not running"
            await emit(tracker.observe(
                reachable: false,
                failovers: seenFailovers,
                openKeys: seenOpen,
                now: now,
                enabled: settings.notificationsEnabled
            ))
        }
    }

    private func emit(_ signals: [NotifySignal]) async {
        guard settings.notificationsEnabled else { return }
        await notifier.deliver(signals)
    }

    private func message(for error: GatewayError) -> String {
        switch error {
        case .invalidBaseURL:
            return "invalid gateway URL"
        case .unreachable:
            return "gateway not running"
        case .rejected(_, let message):
            return message
        }
    }
}
#endif
