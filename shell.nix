{ pkgs ? import <nixpkgs> {} }:

with pkgs; mkShell {
  name = "quickjs-go";
  buildInputs = [
    clang
    llvmPackages.bintools

    # Native build deps
    pkg-config
    openssl
    go
  ];

  LIBCLANG_PATH = pkgs.lib.makeLibraryPath [ pkgs.llvmPackages_latest.libclang.lib ];
}
