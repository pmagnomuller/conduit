import Foundation

extension KeyedDecodingContainer {
    func flexString(_ key: Key) -> String {
        (try? decodeIfPresent(String.self, forKey: key)) ?? ""
    }

    func flexBool(_ key: Key) -> Bool {
        (try? decodeIfPresent(Bool.self, forKey: key)) ?? false
    }

    func flexInt(_ key: Key) -> Int {
        if let value = try? decode(Int.self, forKey: key) {
            return value
        }
        if let value = try? decode(Double.self, forKey: key) {
            return Int(value)
        }
        return 0
    }

    func flexDouble(_ key: Key) -> Double? {
        if let value = try? decode(Double.self, forKey: key) {
            return value
        }
        if let value = try? decode(Int.self, forKey: key) {
            return Double(value)
        }
        return nil
    }

    func flexDate(_ key: Key) -> Date? {
        guard let raw = try? decode(String.self, forKey: key) else { return nil }
        return RFC3339.parse(raw)
    }
}
