{
  description = "MCP server for self-hosted Jira and Bitbucket (Go)";

  # Offer the prebuilt CI cache so consumers fetch the binary instead of
  # rebuilding it. Honoured with --accept-flake-config (or for trusted users).
  # The cache is public; no token needed to pull.
  nixConfig = {
    extra-substituters = [ "https://nix.stubbe.dev/default" ];
    extra-trusted-public-keys = [ "default:9P4FePqHV1rGv5NDBun0GN26y83pcaaMr/NHZrxKaac=" ];
  };

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      # Version tracks the npm wrapper / git tag automatically: it is read from
      # package.json, which `npm run release:*` bumps. No manual edit needed.
      version = (builtins.fromJSON (builtins.readFile ./package.json)).version;

      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: rec {
        atlassian-mcp = pkgs.buildGoModule {
          pname = "atlassian-mcp";
          inherit version;
          src = self;

          # vendorHash is kept current by .github/workflows/flake.yml on any
          # change to go.mod / go.sum. If you bump deps locally, set this to
          # pkgs.lib.fakeHash, run `nix build`, and paste the reported hash.
          vendorHash = "sha256-7C6a+XrlD5krhYLZadyQ59E9RPmCk+ZDFc7PSzBLt3c=";

          # Version is embedded from package.json (single source of truth) — no -X needed.
          ldflags = [ "-s" "-w" ];
          doCheck = true;

          meta = {
            description = "MCP server for self-hosted Jira and Bitbucket (Go)";
            homepage = "https://github.com/stubbedev/atlassian-mcp";
            license = pkgs.lib.licenses.mit;
            mainProgram = "atlassian-mcp";
          };
        };
        default = atlassian-mcp;
      });

      apps = forAllSystems (pkgs: rec {
        atlassian-mcp = {
          type = "app";
          program = "${self.packages.${pkgs.system}.atlassian-mcp}/bin/atlassian-mcp";
        };
        default = atlassian-mcp;
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [ pkgs.go pkgs.gopls pkgs.gotools ];
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixpkgs-fmt);
    };
}
