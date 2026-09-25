import XCTest
@testable import ConduitCore

final class TitleTests: XCTestCase {
    func testAutoIncludesShortModel() {
        XCTAssertEqual(
            menuBarTitle(reachable: true, mode: "auto", forcedProvider: "", lastModel: "claude-opus-5-5"),
            "auto · opus-5-5"
        )
    }

    func testJevAndPinned() {
        XCTAssertEqual(menuBarTitle(reachable: true, mode: "jev", forcedProvider: "", lastModel: "claude-opus-5-5"), "jev")
        XCTAssertEqual(menuBarTitle(reachable: true, mode: "pinned", forcedProvider: "glm", lastModel: ""), "pinned glm")
    }

    func testAutoWithoutModelAndOffline() {
        XCTAssertEqual(menuBarTitle(reachable: true, mode: "auto", forcedProvider: "", lastModel: ""), "auto")
        XCTAssertEqual(menuBarTitle(reachable: false, mode: "auto", forcedProvider: "", lastModel: "claude-opus-5-5"), "offline")
    }

    func testResolvedModeFallsBackToPin() {
        XCTAssertEqual(resolvedMode(routeMode: "", statusMode: "", forcedProvider: "glm"), "pinned")
        XCTAssertEqual(resolvedMode(routeMode: "", statusMode: "", forcedProvider: ""), "auto")
        XCTAssertEqual(resolvedMode(routeMode: "jev", statusMode: "auto", forcedProvider: ""), "jev")
    }
}
