import Foundation

enum RFC3339 {
    /// Go's `time.Time` JSON is RFC3339Nano. Foundation's ISO8601 parser
    /// rejects some fractional lengths, so the fraction is applied separately.
    static func parse(_ raw: String) -> Date? {
        var body = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !body.isEmpty else { return nil }
        var fraction = 0.0
        if let range = body.range(of: #"\.\d+"#, options: .regularExpression) {
            let digits = body[range].dropFirst()
            var scale = 1.0
            for ch in digits.prefix(9) {
                guard let digit = ch.wholeNumberValue else { break }
                scale /= 10
                fraction += Double(digit) * scale
            }
            body.removeSubrange(range)
        }
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime]
        guard let base = formatter.date(from: body) else { return nil }
        return base.addingTimeInterval(fraction)
    }
}
