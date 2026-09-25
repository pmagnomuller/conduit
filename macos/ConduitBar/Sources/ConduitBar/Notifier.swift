#if os(macOS)
import ConduitCore
import Foundation
import UserNotifications

@MainActor
final class Notifier {
    private var asked = false

    func deliver(_ signals: [NotifySignal]) async {
        guard !signals.isEmpty else { return }
        let center = UNUserNotificationCenter.current()
        if !asked {
            asked = true
            let settings = await center.notificationSettings()
            if settings.authorizationStatus == .notDetermined {
                _ = try? await center.requestAuthorization(options: [.alert, .sound])
            }
        }
        let settings = await center.notificationSettings()
        guard settings.authorizationStatus == .authorized else { return }
        for signal in signals {
            let content = UNMutableNotificationContent()
            content.title = "Conduit"
            content.body = signal.body
            let request = UNNotificationRequest(
                identifier: "\(signal.id)-\(UUID().uuidString)",
                content: content,
                trigger: nil
            )
            try? await center.add(request)
        }
    }
}
#endif
