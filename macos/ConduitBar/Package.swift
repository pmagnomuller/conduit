// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "ConduitBar",
    products: [
        .executable(name: "ConduitBar", targets: ["ConduitBar"])
    ],
    targets: [
        .target(name: "ConduitCore"),
        .executableTarget(
            name: "ConduitBar",
            dependencies: ["ConduitCore"]
        ),
        .testTarget(
            name: "ConduitCoreTests",
            dependencies: ["ConduitCore"]
        ),
    ]
)
