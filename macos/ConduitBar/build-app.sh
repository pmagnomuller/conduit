#!/bin/sh
# Assemble ConduitBar.app from a SwiftPM release build. Command Line Tools
# only: no xcodebuild. LSUIElement in Info.plist keeps the app out of the Dock.
set -eu
cd "$(dirname "$0")"

if [ "$(uname)" != "Darwin" ]; then
	echo "macos-app requires macOS (SwiftUI MenuBarExtra)." >&2
	exit 1
fi

arch=$(uname -m)
case "$arch" in
arm64) target="arm64-apple-macos14.0" ;;
x86_64) target="x86_64-apple-macos14.0" ;;
*)
	echo "unsupported architecture: $arch" >&2
	exit 1
	;;
esac

swift build -c release -Xswiftc -target -Xswiftc "$target"
bin="$(swift build -c release -Xswiftc -target -Xswiftc "$target" --show-bin-path)/ConduitBar"

app="build/ConduitBar.app"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS"
cp "$bin" "$app/Contents/MacOS/ConduitBar"
cp Info.plist "$app/Contents/Info.plist"
chmod +x "$app/Contents/MacOS/ConduitBar"
codesign --force --sign - "$app"
echo "built $app"
