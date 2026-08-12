{
  description = "Name herdr tabs after what is running inside them";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: rec {
        herdrtabrenamer = pkgs.buildGoModule {
          pname = "herdrtabrenamer";
          version = "0.1.0";
          src = ./.;
          # No third-party dependencies, so there is nothing to vendor.
          vendorHash = null;
          meta = {
            description = "Automatic tab naming for herdr over its socket API";
            license = pkgs.lib.licenses.mit;
            mainProgram = "herdrtabrenamer";
          };
        };
        default = herdrtabrenamer;
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gopls
          ];
        };
      });

      homeModules.default =
        {
          config,
          lib,
          pkgs,
          ...
        }:
        let
          cfg = config.services.herdrtabrenamer;
        in
        {
          options.services.herdrtabrenamer = {
            enable = lib.mkEnableOption "the herdr automatic tab renamer";
            package = lib.mkOption {
              type = lib.types.package;
              default = self.packages.${pkgs.stdenv.hostPlatform.system}.herdrtabrenamer;
              description = "herdrtabrenamer package to run.";
            };
            format = lib.mkOption {
              type = lib.types.str;
              default = "{proc|dir}";
              example = "{icon} {proc|agent}:{dir}";
              description = "Tab name template.";
            };
            extraArgs = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [ ];
              example = [
                "-poll"
                "5s"
              ];
              description = "Extra command line arguments.";
            };
          };

          config = lib.mkIf cfg.enable {
            systemd.user.services.herdrtabrenamer = {
              Unit = {
                Description = "Automatic tab naming for herdr";
                # The daemon talks to the herdr server socket and reconnects on
                # its own, so ordering only matters at login time.
                After = [ "default.target" ];
              };
              Service = {
                ExecStart = lib.escapeShellArgs (
                  [
                    (lib.getExe cfg.package)
                    "-apply"
                    "-format"
                    cfg.format
                  ]
                  ++ cfg.extraArgs
                );
                # Without a running herdr server the daemon exits immediately;
                # keep retrying so it comes up whenever herdr is started.
                Restart = "always";
                RestartSec = 10;
              };
              Install.WantedBy = [ "default.target" ];
            };
          };
        };
    };
}
