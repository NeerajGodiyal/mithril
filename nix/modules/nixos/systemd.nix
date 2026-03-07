{
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.services.mithril;
  base = import ../shared/systemd-base.nix {inherit config lib pkgs;};
  baseUnit = {
    unitConfig = base.baseUnit.unitConfig or {};
    serviceConfig = base.baseUnit.serviceConfig or {};
  };
in {
  config = lib.mkIf (cfg.enable && !pkgs.stdenv.isDarwin) {
    users.users.${cfg.user} = lib.mkIf (cfg.user != null) {
      isSystemUser = true;
      group = cfg.group;
    };
    users.groups.${cfg.group} = lib.mkIf (cfg.group != null) {};

    systemd.services.mithril =
      baseUnit
      // {
        serviceConfig =
          baseUnit.serviceConfig
          // {
            User = cfg.user;
            Group = cfg.group;
            DynamicUser = false;
            PermissionsStartOnly = true;
          };
        wantedBy = ["multi-user.target"];
      };
  };
}
