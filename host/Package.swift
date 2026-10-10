// swift-tools-version: 6.2
// The thin Swift host: the only Swift in Pomar. It links Apple's
// Containerization and owns VMs; everything else is Go.

import PackageDescription

let package = Package(
    name: "pomar-host",
    platforms: [.macOS("26.0")],
    products: [
        .executable(name: "pomar-host", targets: ["PomarHost"])
    ],
    dependencies: [
        // Pinned exactly; the full graph is pinned by Package.resolved.
        .package(url: "https://github.com/apple/containerization.git", exact: "0.45.0")
    ],
    targets: [
        .target(
            name: "PomarHostCore",
            dependencies: [
                .product(name: "Containerization", package: "containerization"),
                .product(name: "ContainerizationArchive", package: "containerization"),
                .product(name: "ContainerizationEXT4", package: "containerization"),
            ]
        ),
        .executableTarget(
            name: "PomarHost",
            dependencies: ["PomarHostCore"]
        ),
        .testTarget(
            name: "PomarHostTests",
            dependencies: ["PomarHostCore"]
        ),
    ]
)
