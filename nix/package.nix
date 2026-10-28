{
  perSystem = { lib, pkgs, self', ... }: {
    packages = {
      # go.mod requires 1.27; nixpkgs' default buildGoModule lags it, so use
      # the version-pinned builder.
      atlassian-mcp = pkgs.buildGo127Module {
        pname = "atlassian-mcp";
        # Version tracks the npm wrapper / git tag automatically: it is read from
        # package.json, which `npm run release:*` bumps. No manual edit needed.
        inherit (lib.importJSON ../package.json) version;

        src = lib.fileset.toSource {
          root = ../.;
          fileset = lib.fileset.difference (lib.fileset.gitTracked ../.) (
            lib.fileset.unions [
              ../flake.nix
              ../flake.lock
              ../nix
            ]
          );
        };

        # vendorHash is kept current by .github/workflows/flake.yml on any
        # change to go.mod / go.sum. If you bump deps locally, set this to
        # pkgs.lib.fakeHash, run `nix build`, and paste the reported hash.
        vendorHash = "sha256-YtjR+4j5wQrUN/QymmweeN74xOWIT2FQvgV7kkn5sa0=";

        # Version is embedded from package.json (single source of truth) — no -X needed.
        ldflags = [ "-s" "-w" ];
        # Image decoders use their embedded WebAssembly, never system libraries.
        tags = [ "nodynamic" ];
        doCheck = true;

        meta = {
          description = "MCP server for self-hosted Jira and Bitbucket (Go)";
          homepage = "https://github.com/stubbedev/atlassian-mcp";
          license = lib.licenses.mit;
          mainProgram = "atlassian-mcp";
        };
      };
      default = self'.packages.atlassian-mcp;
    };

    apps = {
      atlassian-mcp = {
        type = "app";
        program = lib.getExe self'.packages.atlassian-mcp;
      };
      default = self'.apps.atlassian-mcp;
    };
  };
}
