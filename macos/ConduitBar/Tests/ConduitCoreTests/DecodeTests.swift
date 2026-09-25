import XCTest
@testable import ConduitCore

final class DecodeTests: XCTestCase {
    func testStatusFixture() throws {
        let status = try Fixtures.decode(GatewayStatus.self, Fixtures.status)
        XCTAssertEqual(status.listen, "127.0.0.1:8787")
        XCTAssertEqual(status.routing, "anthropic")
        XCTAssertEqual(status.mode, "auto")
        XCTAssertTrue(status.jevEnabled)
        XCTAssertEqual(status.failoverProvider, "deepseek")
        XCTAssertEqual(status.lastRequest?.provider, "anthropic")
        XCTAssertEqual(status.lastRequest?.upstreamModel, "claude-opus-5-5")
        XCTAssertNotNil(status.lastRequest?.at)
        let entry = status.breaker.entries["anthropic|claude-fable-5-1"]
        XCTAssertEqual(entry?.state, "OPEN")
        XCTAssertEqual(entry?.reason, "quota")
        XCTAssertNotNil(entry?.until)
        XCTAssertEqual(status.breaker.lastQuota?.model, "claude-fable-5-1")
        XCTAssertEqual(status.counts.anthropicRequests, 3)
        XCTAssertEqual(status.counts.glmRequests, 1)
        XCTAssertEqual(status.counts.failovers, 2)
        XCTAssertEqual(status.counts.jevDecisions, 5)
        XCTAssertNotNil(status.counts.startedAt)
        XCTAssertEqual(status.upstream["glm"], "https://api.z.ai/api/anthropic")
    }

    func testRouteAndDecisionFixture() throws {
        let route = try Fixtures.decode(GatewayRoute.self, Fixtures.route)
        XCTAssertEqual(route.mode, "jev")
        XCTAssertEqual(route.available["anthropic"], ["claude-opus-5-5", "claude-sonnet-5"])
        XCTAssertEqual(route.jev?.enabled, true)
        XCTAssertEqual(route.jev?.catalog.first?.model, "glm-5.3")
        let decision = try XCTUnwrap(route.jev?.recent.first)
        XCTAssertEqual(decision.requestedModel, "claude-opus-5-5")
        XCTAssertEqual(decision.provider, "glm")
        XCTAssertEqual(decision.model, "glm-5.3")
        XCTAssertEqual(decision.step, "user_turn")
        XCTAssertEqual(decision.lease, "one_call")
        XCTAssertEqual(decision.source, "jev")
        XCTAssertEqual(decision.reason, "capability")
        XCTAssertEqual(decision.confidence ?? 0, 0.82, accuracy: 0.001)
        XCTAssertEqual(decision.margin ?? 0, 0.2, accuracy: 0.001)
        XCTAssertEqual(decision.pick, "glm-5.3")
        XCTAssertEqual(decision.estInputUSD ?? 0, 0.01, accuracy: 0.0001)
        XCTAssertEqual(decision.baselineInputUSD ?? 0, 0.04, accuracy: 0.0001)
        XCTAssertEqual(decision.latencyMS, 120)
        XCTAssertNotNil(decision.at)
    }

    func testMissingFieldsDecode() throws {
        let status = try Fixtures.decode(GatewayStatus.self, "{}")
        XCTAssertEqual(status.mode, "")
        XCTAssertFalse(status.jevEnabled)
        XCTAssertNil(status.lastRequest)
        XCTAssertTrue(status.breaker.entries.isEmpty)
        XCTAssertEqual(status.counts.failovers, 0)

        let route = try Fixtures.decode(GatewayRoute.self, #"{"mode":"auto"}"#)
        XCTAssertEqual(route.mode, "auto")
        XCTAssertNil(route.jev)
        XCTAssertTrue(route.available.isEmpty)
    }

    func testRouteCommandsEncode() throws {
        func object(_ command: RouteCommand) throws -> [String: Any] {
            let data = try JSONEncoder().encode(command)
            return try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
        }
        XCTAssertEqual(try object(.auto)["mode"] as? String, "auto")
        XCTAssertEqual(try object(.jev)["mode"] as? String, "jev")
        XCTAssertEqual(try object(.clear)["clear"] as? Bool, true)
        let pin = try object(.pin(provider: "anthropic", model: "claude-opus-5-5"))
        XCTAssertEqual(pin["provider"] as? String, "anthropic")
        XCTAssertEqual(pin["model"] as? String, "claude-opus-5-5")
        let bare = try object(.pin(provider: "glm", model: nil))
        XCTAssertEqual(bare["provider"] as? String, "glm")
        XCTAssertNil(bare["model"])
    }
}
