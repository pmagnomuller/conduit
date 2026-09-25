#if !os(macOS)
import Foundation

@main
enum ConduitBarMain {
    static func main() {
        FileHandle.standardError.write(Data("ConduitBar is a macOS menu bar app.\n".utf8))
        exit(1)
    }
}
#endif
