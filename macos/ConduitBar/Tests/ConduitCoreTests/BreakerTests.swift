import XCTest
@testable import ConduitCore

final class BreakerTests: XCTestCase {
    private func entry(state: String, until: Date?) throws -> BreakerEntry {
        var object: [String: String] = ["state": state, "reason": "quota"]
        if let until {
            object["until"] = iso(until)
        }
        let data = try JSONSerialization.data(withJSONObject: object)
        return try JSONDecoder().decode(BreakerEntry.self, from: data)
    }

    private func iso(_ date: Date) -> String {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime]
        return formatter.string(from: date)
    }

    func testOpenUntilFutureIsOpen() throws {
        let now = Date(timeIntervalSince1970: 1_000)
        let entry = try entry(state: "OPEN", until: now.addingTimeInterval(30))
        XCTAssertTrue(isTrulyOpen(entry, now: now))
        XCTAssertEqual(breakerLabel(entry, now: now), "open")
        XCTAssertEqual(countdown(until: entry.until!, now: now), "in 30s")
    }

    func testStaleOpenIsExpired() throws {
        let now = Date(timeIntervalSince1970: 1_000)
        let entry = try entry(state: "OPEN", until: now.addingTimeInterval(-5))
        XCTAssertFalse(isTrulyOpen(entry, now: now))
        XCTAssertEqual(breakerLabel(entry, now: now), "expired")
        XCTAssertEqual(countdown(until: entry.until!, now: now), "expired")
        XCTAssertTrue(trulyOpenKeys(["anthropic|claude-fable-5-1": entry], now: now).isEmpty)
    }

    func testOpenWithoutUntilStaysOpen() throws {
        let now = Date()
        let entry = try entry(state: "OPEN", until: nil)
        XCTAssertTrue(isTrulyOpen(entry, now: now))
        XCTAssertEqual(trulyOpenKeys(["k": entry], now: now), ["k"])
    }

    func testHalfOpenIsNotOpen() throws {
        let now = Date()
        let entry = try entry(state: "HALF_OPEN", until: now.addingTimeInterval(10))
        XCTAssertFalse(isTrulyOpen(entry, now: now))
        XCTAssertEqual(breakerLabel(entry, now: now), "half_open")
    }
}
