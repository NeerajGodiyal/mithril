{self}: {
  imports = [
    (import ../shared/options.nix {inherit self;})
    ./systemd.nix
    ./storage.nix
    ./service.nix
  ];
}
