import Foundation
@testable import ConduitCore
import XCTest

enum Fixtures {
    static let status = """
    {
      "listen": "127.0.0.1:8787",
      "routing": "anthropic",
      "mode": "auto",
      "jev_enabled": true,
      "failover_provider": "deepseek",
      "last_request": {"at": "2026-09-25T07:40:00.123456789Z", "provider": "anthropic", "upstream_model": "claude-opus-5-5"},
      "breaker": {
        "entries": {
          "anthropic|claude-fable-5-1": {
            "state": "OPEN",
            "until": "2026-09-25T08:00:00Z",
            "reason": "quota",
            "opened_at": "2026-09-25T07:30:00Z",
            "last_change": "2026-09-25T07:30:00Z"
          }
        },
        "last_quota": {"at": "2026-09-25T07:30:00Z", "model": "claude-fable-5-1", "reason": "quota"},
        "mode": "auto"
      },
      "counts": {
        "anthropic_requests": 3,
        "glm_requests": 1,
        "deepseek_requests": 0,
        "failovers": 2,
        "transient_retries": 4,
        "jev_decisions": 5,
        "jev_fail_open": 1,
        "started_at": "2026-09-25T07:00:00Z"
      },
      "upstream": {"anthropic": "https://api.anthropic.com", "glm": "https://api.z.ai/api/anthropic", "deepseek": "https://api.deepseek.com/anthropic"},
      "future_field": {"ignored": true}
    }
    """

    static let route = """
    {
      "mode": "jev",
      "forced_provider": "",
      "forced_model": "",
      "available": {
        "anthropic": ["claude-opus-5-5", "claude-sonnet-5"],
        "glm": ["glm-5.3"],
        "deepseek": ["deepseek-v4-pro"]
      },
      "jev": {
        "enabled": true,
        "catalog": [{"provider": "glm", "model": "glm-5.3", "profile": "fast"}],
        "recent": [{
          "at": "2026-09-25T07:41:00Z",
          "requested_model": "claude-opus-5-5",
          "provider": "glm",
          "model": "glm-5.3",
          "step": "user_turn",
          "lease": "one_call",
          "source": "jev",
          "reason": "capability",
          "confidence": 0.82,
          "margin": 0.2,
          "pick": "glm-5.3",
          "policy": "",
          "est_input_usd": 0.01,
          "baseline_input_usd": 0.04,
          "latency_ms": 120,
          "extra": "ignored"
        }]
      },
      "last_request": {"at": "2026-09-25T07:40:00Z", "provider": "anthropic", "upstream_model": "claude-opus-5-5"}
    }
    """

    static func decode<T: Decodable>(_ type: T.Type, _ json: String) throws -> T {
        try JSONDecoder().decode(type, from: Data(json.utf8))
    }
}
