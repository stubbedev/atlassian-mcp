{
  description = "MCP server for self-hosted Jira and Bitbucket (Go)";

  # Offer the prebuilt CI cache so consumers fetch the binary instead of
  # rebuilding it. Honoured with --accept-flake-config (or for trusted users).
  # The cache is public; no token needed to pull.
  nixConfig = {
    extra-substituters = [
      "https://nix.stubbe.dev/c/default/default"
      "https://nix.stubbe.dev/c/default/flakes"
    ];
    extra-trusted-public-keys = [
      "default:6uWvXutL9cXjV3lii+Ur5ff+ArQoG4kMBKNXWrIxhHg="
      "flakes:0Dr9MyPOvA6va2GVzflJcbrChNoZUeIZflyrww3O+YM="
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
