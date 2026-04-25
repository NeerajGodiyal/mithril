{
  config,
  lib,
  pkgs,
  ...
}: let
  base = import ../shared/systemd-base.nix {inherit config lib pkgs;};
in {
  config = lib.mkIf (config.services.mithril.enable && !pkgs.stdenv.isDarwin) {
    systemd.user.services.mithril =
      base.baseUnit
      // {
        wantedBy = ["default.target"];
      };
  };
}
