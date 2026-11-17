{
  description = "MCP server for self-hosted Jira and Bitbucket (Go)";

  # Offer the prebuilt CI cache so consumers fetch the binary instead of
  # rebuilding it. Honoured with --accept-flake-config (or for trusted users).
  # The cache is public; no token needed to pull.
  nixConfig = {
    extra-substituters = [
      "https://xilos.dev/c/stubbedev/main"
    ];
    extra-trusted-public-keys = [
      "xilos.dev/stubbedev/main-1:397R6S49v/FSvP+mZ3HlKrVN1KXhgzHIEIHxA/UuG6Y="
    ];
  };

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts = {
      url = "github:hercules-ci/flake-parts";
      inputs.nixpkgs-lib.follows = "nixpkgs";
    };
    devtools = {
      url = "github:stubbedev/flakes.nix";
      inputs.flake-parts.follows = "flake-parts";
    };
  };

  outputs = inputs @ { flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ];
      imports = [
        ./nix/package.nix
        ./nix/shell.nix
      ];
    };
}
