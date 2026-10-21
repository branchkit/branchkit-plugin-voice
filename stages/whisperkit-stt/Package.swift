// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "whisperkit-stt-stage",
    platforms: [.macOS(.v14)],
    dependencies: [
        // upToNextMinor, not `from:`. WhisperKit is pre-1.0, so SPM's `from:`
        // admits every 0.x — this manifest said 0.15.0 while the build had
        // silently moved to 0.18.0. That matters more here than in a normal
        // dependency: this stage's no-network property is a PRODUCT claim, it
        // is enforced by a sandbox profile tuned to what this version does,
        // and the transitive swift-transformers is what fetches tokenizers
        // from Hugging Face. A minor bump must be a deliberate, re-verified
        // act (0.15.0 had drifted to 0.18.0 invisibly while Package.resolved was
        // gitignored), not a side effect of
        // resolving on a new machine. Package.resolved is committed alongside.
        .package(url: "https://github.com/argmaxinc/WhisperKit.git", .upToNextMinor(from: "0.18.0")),
    ],
    targets: [
        .executableTarget(
            name: "whisperkit_stage",
            dependencies: [
                .product(name: "WhisperKit", package: "WhisperKit"),
                "TranscriptScoring",
                "StageLog",
            ],
            path: "Sources/whisperkit_stage"
        ),
        // The structured stage-log line and the [PERF] line, also free of
        // WhisperKit for the same reason.
        .target(name: "StageLog", path: "Sources/StageLog"),
        .testTarget(
            name: "StageLogTests",
            dependencies: ["StageLog"],
            path: "Tests/StageLogTests"
        ),
        // Pure functions over a transcript's tokens, kept free of WhisperKit
        // so `swift test` checks them without a model.
        .target(name: "TranscriptScoring", path: "Sources/TranscriptScoring"),
        .testTarget(
            name: "TranscriptScoringTests",
            dependencies: ["TranscriptScoring"],
            path: "Tests/TranscriptScoringTests"
        ),
    ]
)
