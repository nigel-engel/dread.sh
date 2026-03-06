import Cocoa
import UserNotifications

class AppDelegate: NSObject, NSApplicationDelegate, UNUserNotificationCenterDelegate {
    func userNotificationCenter(_ center: UNUserNotificationCenter,
                                willPresent notification: UNNotification,
                                withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
        completionHandler([.banner, .sound])
    }
}

let args = CommandLine.arguments
var title = "dread.sh"
var message = ""
var soundName = "Sosumi"

var i = 1
while i < args.count {
    switch args[i] {
    case "-title" where i + 1 < args.count:
        i += 1; title = args[i]
    case "-message" where i + 1 < args.count:
        i += 1; message = args[i]
    case "-sound" where i + 1 < args.count:
        i += 1; soundName = args[i]
    default: break
    }
    i += 1
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate

let center = UNUserNotificationCenter.current()
center.delegate = delegate

let semaphore = DispatchSemaphore(value: 0)

center.requestAuthorization(options: [.alert, .sound]) { granted, _ in
    guard granted else {
        semaphore.signal()
        return
    }

    let content = UNMutableNotificationContent()
    content.title = title
    content.body = message
    content.sound = UNNotificationSound(named: UNNotificationSoundName(soundName))

    let request = UNNotificationRequest(identifier: UUID().uuidString, content: content, trigger: nil)
    center.add(request) { _ in
        semaphore.signal()
    }
}

_ = semaphore.wait(timeout: .now() + 5)
