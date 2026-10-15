# Proposed release staging correction

This is the review artifact for the applied staging correction. The diff records the change from the original script.

The regression TestReleaseBuildFailurePreservesPreviousAssets failed against the original script and passes after the correction.
An injected second-platform failure replaces the old macOS asset and removes the Linux asset.
The existing checksum file remains unchanged. The reproduction exits 42.

The proposed script builds both targets in one hidden temporary directory on the repository filesystem.
All asset and checksum moves follow the successful build loop. The exit trap removes only the created directory.
The output directory remains present. Target names, version injection, toolchain and checksum format stay unchanged.

```diff
--- a/scripts/build-release
+++ b/scripts/build-release
@@ -13,18 +13,21 @@
 [ "$(GOTOOLCHAIN=local go env GOVERSION)" = "$toolchain" ] ||
 	{ echo "build-release: need $toolchain, have $(GOTOOLCHAIN=local go env GOVERSION)" >&2; exit 1; }
 
-rm -rf dist
-mkdir dist
-pins=$(mktemp)
-trap 'rm -f "$pins"' EXIT
+build_dir=$(mktemp -d "$root/.release-build.XXXXXX")
+trap 'rm -rf "$build_dir"' EXIT
+pins="$build_dir/pins"
 for target in darwin/arm64 linux/amd64; do
 	asset="agent-decisions-${target%/*}-${target#*/}"
 	GOOS=${target%/*} GOARCH=${target#*/} CGO_ENABLED=0 GOFLAGS= GOTOOLCHAIN=local \
 		go build -trimpath -buildvcs=false -ldflags "-s -w -buildid= -X main.version=$version" \
-		-o "dist/$asset" ./cmd/agent-decisions
-	(cd dist && shasum -a 256 "$asset") >>"$pins"
+		-o "$build_dir/$asset" ./cmd/agent-decisions
+	(cd "$build_dir" && shasum -a 256 "$asset") >>"$pins"
 done
+mkdir -p dist
+for target in darwin/arm64 linux/amd64; do
+	asset="agent-decisions-${target%/*}-${target#*/}"
+	mv "$build_dir/$asset" "dist/$asset"
+done
+chmod 644 "$pins"
 mv "$pins" bin/agent-decisions.sha256
-trap - EXIT
-chmod 644 bin/agent-decisions.sha256
 cat bin/agent-decisions.sha256
```
