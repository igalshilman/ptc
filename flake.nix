{
  description = "quickjs-worker-go — a durable CodeAct AI agent on Restate";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      # Systems we ship a dev shell for.
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          name = "quickjs-worker-go";

          # `go build ./...` and `go test ./...` need only Go — the QuickJS guest is a
          # committed prebuilt .wasm. Rust (via rustup, which honours the pinned
          # rust-toolchain.toml + auto-installs the wasm32-wasip1 target) is only needed
          # to rebuild the guest with `make guest-rs`.
          packages = with pkgs; [
            go            # 1.25+
            gopls
            gotools

            rustup        # resolves the channel/target pinned in rust-toolchain.toml
            clang         # C linker for the wasm build
            pkg-config
          ];

          shellHook = ''
            echo "quickjs-worker-go dev shell — go $(go version | awk '{print $3}')"
            echo "  go build ./...      # engine + both examples"
            echo "  go test ./...       # offline test suite"
            echo "  make guest-rs       # rebuild agent/quickjs_guest.wasm (needs Rust)"
          '';
        };
      });
    };
}
