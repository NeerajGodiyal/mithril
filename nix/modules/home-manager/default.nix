{self}: {
  imports = [
    (import ../shared/options.nix {inherit self;})
    ./systemd.nix
    ./service.nix
  ];
}
