{
  config,
  lib,
  pkgs,
  ...
}: let
  cfg = config.services.mithril;
  escapeSystemdPath = path: let
    stripped = lib.removePrefix "/" path;
    escaped = lib.replaceStrings ["-"] ["\\x2d"] stripped;
  in
    lib.replaceStrings ["/"] ["-"] escaped;
  dirName = "mithril";
  stateDir = "/var/lib/${dirName}";
  accountsMountUnit =
    if cfg.storage.accounts.mountPoint != null
    then "${escapeSystemdPath cfg.storage.accounts.mountPoint}.mount"
    else null;
  blocksMountUnit =
    if cfg.storage.blocks.mountPoint != null
    then "${escapeSystemdPath cfg.storage.blocks.mountPoint}.mount"
    else null;
  singleMountUnit =
    if cfg.storage.singleDisk.mountPoint != null
    then "${escapeSystemdPath cfg.storage.singleDisk.mountPoint}.mount"
    else null;
in {
  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion =
          !(
            cfg.storage.singleDisk.enable
            && (cfg.storage.accounts.device != null || cfg.storage.blocks.device != null)
          );
        message = "services.mithril.storage.singleDisk.enable cannot be used with storage.accounts.device or storage.blocks.device.";
      }
      {
        assertion = !(cfg.storage.singleDisk.enable && cfg.storage.singleDisk.device == null);
        message = "services.mithril.storage.singleDisk.enable requires storage.singleDisk.device.";
      }
      {
        assertion = !(cfg.storage.singleDisk.enable && cfg.storage.singleDisk.mountPoint == null);
        message = "services.mithril.storage.singleDisk.enable requires storage.singleDisk.mountPoint.";
      }
      {
        assertion = !(cfg.storage.accounts.device != null && cfg.storage.accounts.mountPoint == null);
        message = "services.mithril.storage.accounts.device requires storage.accounts.mountPoint.";
      }
      {
        assertion = !(cfg.storage.blocks.device != null && cfg.storage.blocks.mountPoint == null);
        message = "services.mithril.storage.blocks.device requires storage.blocks.mountPoint.";
      }
    ];

    services.mithril.storage = {
      singleDisk.mountPoint = lib.mkIf cfg.storage.singleDisk.enable (
        lib.mkDefault stateDir
      );
      accounts.mountPoint = lib.mkDefault (
        if cfg.storage.singleDisk.enable
        then "${cfg.storage.singleDisk.mountPoint}/accounts"
        else "${stateDir}/accounts"
      );
      blocks.mountPoint = lib.mkDefault (
        if cfg.storage.singleDisk.enable
        then "${cfg.storage.singleDisk.mountPoint}/blocks"
        else "${stateDir}/blocks"
      );
    };

    systemd.tmpfiles.rules =
      lib.optional cfg.storage.singleDisk.enable "d ${cfg.storage.singleDisk.mountPoint} 0755 root root - -"
      ++ lib.optional (
        cfg.storage.singleDisk.enable || cfg.storage.accounts.device != null
      ) "d ${cfg.storage.accounts.mountPoint} 0755 root root - -"
      ++ lib.optional (
        cfg.storage.singleDisk.enable || cfg.storage.blocks.device != null
      ) "d ${cfg.storage.blocks.mountPoint} 0755 root root - -";

    fileSystems = lib.mkMerge [
      (lib.mkIf cfg.storage.singleDisk.enable {
        ${cfg.storage.singleDisk.mountPoint} = {
          inherit (cfg.storage.singleDisk) device;
          inherit (cfg.storage.singleDisk) fsType;
          options = cfg.storage.singleDisk.mountOptions;
        };
      })
      (lib.mkIf (cfg.storage.accounts.device != null) {
        ${cfg.storage.accounts.mountPoint} = {
          inherit (cfg.storage.accounts) device;
          inherit (cfg.storage.accounts) fsType;
          options = cfg.storage.accounts.mountOptions;
        };
      })
      (lib.mkIf (cfg.storage.blocks.device != null) {
        ${cfg.storage.blocks.mountPoint} = {
          inherit (cfg.storage.blocks) device;
          inherit (cfg.storage.blocks) fsType;
          options = cfg.storage.blocks.mountOptions;
        };
      })
    ];

    systemd.services = lib.mkMerge [
      (lib.mkIf (cfg.storage.singleDisk.enable && cfg.storage.singleDisk.format.enable) {
        mithril-format-single = {
          description = "Format Mithril single disk if needed";
          before = [singleMountUnit];
          requiredBy = [singleMountUnit];
          serviceConfig = {
            Type = "oneshot";
            RemainAfterExit = true;
          };
          path = [
            pkgs.util-linux
            pkgs.e2fsprogs
            pkgs.xfsprogs
            pkgs.f2fs-tools
          ];
          script = ''
            set -euo pipefail
            device="${cfg.storage.singleDisk.device}"
            fstype="${cfg.storage.singleDisk.fsType}"
            label="${cfg.storage.singleDisk.format.label}"
            existing="$(blkid -o value -s TYPE "$device" || true)"
            if [ -n "$existing" ] && [ "${
              if cfg.storage.singleDisk.format.force
              then "true"
              else "false"
            }" != "true" ]; then
              echo "Single disk already formatted as $existing. Skipping."
              exit 0
            fi
            if [ "${
              if cfg.storage.singleDisk.format.force
              then "true"
              else "false"
            }" = "true" ]; then
              wipefs -a "$device" || true
            fi
            case "$fstype" in
              ext4) mkfs.ext4 -F -L "$label" "$device" ;;
              xfs)  mkfs.xfs -f -L "$label" "$device" ;;
              f2fs) mkfs.f2fs -f -l "$label" "$device" ;;
            esac
          '';
        };
      })
      (lib.mkIf (cfg.storage.accounts.device != null && cfg.storage.accounts.format.enable) {
        mithril-format-accounts = {
          description = "Format Mithril AccountsDB disk if needed";
          before = [accountsMountUnit];
          requiredBy = [accountsMountUnit];
          serviceConfig = {
            Type = "oneshot";
            RemainAfterExit = true;
          };
          path = [
            pkgs.util-linux
            pkgs.e2fsprogs
            pkgs.xfsprogs
            pkgs.f2fs-tools
          ];
          script = ''
            set -euo pipefail
            device="${cfg.storage.accounts.device}"
            fstype="${cfg.storage.accounts.fsType}"
            label="${cfg.storage.accounts.format.label}"
            existing="$(blkid -o value -s TYPE "$device" || true)"
            if [ -n "$existing" ] && [ "${
              if cfg.storage.accounts.format.force
              then "true"
              else "false"
            }" != "true" ]; then
              echo "Accounts disk already formatted as $existing. Skipping."
              exit 0
            fi
            if [ "${
              if cfg.storage.accounts.format.force
              then "true"
              else "false"
            }" = "true" ]; then
              wipefs -a "$device" || true
            fi
            case "$fstype" in
              ext4) mkfs.ext4 -F -L "$label" "$device" ;;
              xfs)  mkfs.xfs -f -L "$label" "$device" ;;
              f2fs) mkfs.f2fs -f -l "$label" "$device" ;;
            esac
          '';
        };
      })
      (lib.mkIf (cfg.storage.blocks.device != null && cfg.storage.blocks.format.enable) {
        mithril-format-blocks = {
          description = "Format Mithril blocks disk if needed";
          before = [blocksMountUnit];
          requiredBy = [blocksMountUnit];
          serviceConfig = {
            Type = "oneshot";
            RemainAfterExit = true;
          };
          path = [
            pkgs.util-linux
            pkgs.e2fsprogs
            pkgs.xfsprogs
            pkgs.f2fs-tools
          ];
          script = ''
            set -euo pipefail
            device="${cfg.storage.blocks.device}"
            fstype="${cfg.storage.blocks.fsType}"
            label="${cfg.storage.blocks.format.label}"
            existing="$(blkid -o value -s TYPE "$device" || true)"
            if [ -n "$existing" ] && [ "${
              if cfg.storage.blocks.format.force
              then "true"
              else "false"
            }" != "true" ]; then
              echo "Blocks disk already formatted as $existing. Skipping."
              exit 0
            fi
            if [ "${
              if cfg.storage.blocks.format.force
              then "true"
              else "false"
            }" = "true" ]; then
              wipefs -a "$device" || true
            fi
            case "$fstype" in
              ext4) mkfs.ext4 -F -L "$label" "$device" ;;
              xfs)  mkfs.xfs -f -L "$label" "$device" ;;
              f2fs) mkfs.f2fs -f -l "$label" "$device" ;;
            esac
          '';
        };
      })
    ];
  };
}
