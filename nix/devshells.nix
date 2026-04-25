{
  self,
  nixpkgs,
  packages,
}: {
  devShells = packages.forAllSystems (
    system: let
      pkgs = import nixpkgs {inherit system;};
      go = packages.goFor pkgs;
    in {
      default = pkgs.mkShell {
        inputsFrom = [self.packages.${system}.mithril];
        packages = [
          go
          pkgs.gnumake
          pkgs.pkg-config
          pkgs.zstd
          pkgs.gcc
        ];
      };
    }
  );
}
