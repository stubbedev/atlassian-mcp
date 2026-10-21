{
  perSystem = { pkgs, self', ... }: {
    devShells.default = pkgs.mkShell {
      inputsFrom = [ self'.packages.default ];
      packages = [ pkgs.gopls pkgs.gotools pkgs.golangci-lint ];
    };

    formatter = pkgs.nixpkgs-fmt;
  };
}
