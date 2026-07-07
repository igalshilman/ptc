{ pkgs ? import <nixpkgs> {} }:

with pkgs; mkShell {
  name = "quickjs-go";
  buildInputs = [
    clang
    llvmPackages.bintools

    # rust
    rustup
		cargo
		clang
		cmake
		pkg-config

    # Native build deps
    pkg-config
    openssl
    go
  ];

  LIBCLANG_PATH = pkgs.lib.makeLibraryPath [ pkgs.llvmPackages_latest.libclang.lib ];

  # Workaround: Apple Clang's hardened libc++ bounds-checks on unique_ptr<T[]>
	# are incompatible with RocksDB's custom cache-line-aligned allocator.
	# See rocksdb-apple-clang-trap.md for details.
	CFLAGS = "-D_LIBCPP_HARDENING_MODE=_LIBCPP_HARDENING_MODE_NONE";
	CXXFLAGS = "-D_LIBCPP_HARDENING_MODE=_LIBCPP_HARDENING_MODE_NONE";

}
