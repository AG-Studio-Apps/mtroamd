{
  description = "mtroamd — persistent terminal sessions for meshTerm over QUIC/mtRoam, plus the mtroam CLI";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    let
      # Bump per release (mirrors packaging/aur/*/PKGBUILD pkgver). The
      # build date is pinned, not stamped from the clock, so the derivation
      # stays reproducible; Commit comes from the flake's own git rev.
      version = "1.4.10";

      # ── per-system outputs (package, app, devShell, checks) ──────────────
      perSystem = flake-utils.lib.eachDefaultSystem (system:
        let
          pkgs = import nixpkgs { inherit system; };

          mtroamd = pkgs.buildGoModule {
            pname = "mtroamd";
            inherit version;
            src = self;

            # No vendor/ dir in-tree → buildGoModule fetches deps and pins
            # them by hash. Re-lock (fakeHash → build → paste "got:") whenever
            # go.mod/go.sum change.
            vendorHash = "sha256-6+ZtnjwY5rvjLU8zNMuoyHg+8tiUqMvqHOrxsOjDCdc=";

            subPackages = [ "cmd/mtroamd" "cmd/mtroam" ];

            tags = [ "netgo" ];
            ldflags = [
              "-s"
              "-w"
              "-X github.com/AG-Studio-Apps/mtroamd/internal/build.Version=v${version}"
              "-X github.com/AG-Studio-Apps/mtroamd/internal/build.Commit=${self.shortRev or self.dirtyShortRev or "nix"}"
              "-X github.com/AG-Studio-Apps/mtroamd/internal/build.Date=1970-01-01T00:00:00Z"
            ];

            nativeBuildInputs = [ pkgs.pandoc pkgs.installShellFiles ];

            # Mirror the Makefile `manpages` + `completions` targets so the
            # Nix package ships the same docs/completions as the .deb/AUR.
            postInstall = ''
              pandoc -s -t man docs/man/mtroamd.8.md -o mtroamd.8
              pandoc -s -t man docs/man/mtroam.1.md  -o mtroam.1
              installManPage mtroamd.8 mtroam.1

              for bin in mtroamd mtroam; do
                go run ./cmd/gen-completions -shell bash -binary "$bin" > "$bin.bash"
                go run ./cmd/gen-completions -shell zsh  -binary "$bin" > "$bin.zsh"
                go run ./cmd/gen-completions -shell fish -binary "$bin" > "$bin.fish"
                installShellCompletion --cmd "$bin" \
                  --bash "$bin.bash" --zsh "$bin.zsh" --fish "$bin.fish"
              done
            '';

            meta = with pkgs.lib; {
              description = "Persistent terminal daemon over QUIC/mtRoam, with the mtroam CLI";
              homepage = "https://github.com/AG-Studio-Apps/mtroamd";
              license = licenses.agpl3Plus;
              mainProgram = "mtroamd";
              platforms = platforms.unix;
            };
          };
        in
        {
          packages.default = mtroamd;
          packages.mtroamd = mtroamd;
          apps.default = flake-utils.lib.mkApp { drv = mtroamd; name = "mtroamd"; };
          devShells.default = pkgs.mkShell {
            packages = [ pkgs.go pkgs.pandoc ];
          };
          # `nix flake check` builds the package and (on Linux) runs a NixOS VM
          # test of the module: service active, linger on, doctor reports
          # systemd-user (also exercises the multi-dir unit detection on NixOS,
          # where the unit lives in /etc/systemd/user).
          checks = {
            mtroamd = mtroamd;
          } // pkgs.lib.optionalAttrs pkgs.stdenv.isLinux {
            nixos-module = pkgs.testers.nixosTest {
              name = "mtroamd-nixos-module";
              nodes.machine = { ... }: {
                imports = [ self.nixosModules.default ];
                services.mtroamd = {
                  enable = true;
                  users = [ "alice" ];
                  package = mtroamd;
                  tcpAddr = "-"; # QUIC-only; no tailnet poll in the test VM
                };
                users.users.alice = {
                  isNormalUser = true;
                  uid = 1000;
                };
              };
              testScript = ''
                machine.wait_for_unit("multi-user.target")
                # The module enabled linger for alice.
                machine.wait_until_succeeds(
                    "loginctl show-user alice --property=Linger | grep -q Linger=yes"
                )
                # alice's systemd --user service is active.
                machine.wait_until_succeeds(
                    "systemctl --user --machine=alice@.host is-active mtroamd",
                    timeout=90,
                )
                # doctor detects the systemd-user backend (unit in /etc/systemd/user).
                machine.succeed(
                    "su alice -c 'XDG_RUNTIME_DIR=/run/user/1000 "
                    "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus "
                    "mtroamd doctor' | grep -E 'Backend:.*systemd-user'"
                )
              '';
            };
          };
        });

      # ── NixOS system module ──────────────────────────────────────────────
      # Defines the per-user mtroamd `systemd --user` service and (for the
      # listed users) enables linger so it survives logout + reboot — the
      # declarative equivalent of the apt/dnf postinstall auto-setup.
      nixosModule = { config, lib, pkgs, ... }:
        let
          cfg = config.services.mtroamd;
          portOf = a: lib.toInt (lib.last (lib.splitString ":" a));
          tcpArg = lib.optionalString (cfg.tcpAddr != "-") "--mtroam-tcp-addr ${cfg.tcpAddr} ";
        in
        {
          options.services.mtroamd = {
            enable = lib.mkEnableOption "the mtroamd roaming terminal daemon (systemd --user)";
            package = lib.mkOption {
              type = lib.types.package;
              default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
              defaultText = lib.literalExpression "mtroamd.packages.\${system}.default";
              description = "The mtroamd package to run.";
            };
            users = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [ ];
              example = [ "alice" ];
              description = ''
                Users whose mtroamd `--user` service should survive logout +
                reboot. Sets `users.users.<name>.linger = true` for each. The
                service unit itself is defined for all users; lingering is what
                makes it run at boot without an interactive login.
              '';
            };
            addr = lib.mkOption {
              type = lib.types.str;
              default = "0.0.0.0:49820";
              description = "QUIC bind address (host:port).";
            };
            tcpAddr = lib.mkOption {
              type = lib.types.str;
              default = "tailnet:49920";
              description = ''
                mtRoam-over-TCP listener. The `tailnet:<port>` sentinel binds a
                Tailscale interface IP once available. Set to `"-"` to run
                QUIC-only.
              '';
            };
            socket = lib.mkOption {
              type = lib.types.str;
              default = "%h/.local/share/mtroamd/mtroamd.sock";
              description = "IPC socket path (systemd %h expands to the user's home).";
            };
            openFirewall = lib.mkOption {
              type = lib.types.bool;
              default = false;
              description = "Open the QUIC UDP port in the firewall.";
            };
          };

          config = lib.mkIf cfg.enable (lib.mkMerge [
            {
              # NOTE: KillMode=process is load-bearing — it keeps the
              # per-session pty-sidecar children alive across a restart. Keep
              # in sync with internal/svcmgr/unitfile.go (the unit SSOT).
              systemd.user.services.mtroamd = {
                description = "mtroamd — meshTerm roaming daemon";
                documentation = [ "https://github.com/AG-Studio-Apps/mtroamd" ];
                after = [ "network.target" ];
                wantedBy = [ "default.target" ];
                serviceConfig = {
                  Type = "simple";
                  ExecStart = "${cfg.package}/bin/mtroamd serve --addr ${cfg.addr} ${tcpArg}--socket ${cfg.socket}";
                  Restart = "on-failure";
                  RestartSec = 5;
                  KillMode = "process";
                };
              };
              environment.systemPackages = [ cfg.package ];
            }
            (lib.mkIf (cfg.users != [ ]) {
              users.users = lib.genAttrs cfg.users (_: { linger = true; });
            })
            (lib.mkIf cfg.openFirewall {
              networking.firewall.allowedUDPPorts = [ (portOf cfg.addr) ];
            })
          ]);
        };

      # ── home-manager module ──────────────────────────────────────────────
      # Per-user systemd --user service. home-manager can't set linger (a
      # system-level setting), so it warns the user to do so for boot-survival.
      hmModule = { config, lib, pkgs, ... }:
        let
          cfg = config.services.mtroamd;
          tcpArg = lib.optionalString (cfg.tcpAddr != "-") "--mtroam-tcp-addr ${cfg.tcpAddr} ";
        in
        {
          options.services.mtroamd = {
            enable = lib.mkEnableOption "the mtroamd roaming terminal daemon (systemd --user)";
            package = lib.mkOption {
              type = lib.types.package;
              default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
              defaultText = lib.literalExpression "mtroamd.packages.\${system}.default";
              description = "The mtroamd package to run.";
            };
            addr = lib.mkOption {
              type = lib.types.str;
              default = "0.0.0.0:49820";
              description = "QUIC bind address (host:port).";
            };
            tcpAddr = lib.mkOption {
              type = lib.types.str;
              default = "tailnet:49920";
              description = "mtRoam-over-TCP listener; `\"-\"` for QUIC-only.";
            };
            socket = lib.mkOption {
              type = lib.types.str;
              default = "%h/.local/share/mtroamd/mtroamd.sock";
              description = "IPC socket path.";
            };
          };

          config = lib.mkIf cfg.enable {
            home.packages = [ cfg.package ];
            # KillMode=process: keep pty-sidecar children alive across restart
            # (matches internal/svcmgr/unitfile.go).
            systemd.user.services.mtroamd = {
              Unit = {
                Description = "mtroamd — meshTerm roaming daemon";
                Documentation = "https://github.com/AG-Studio-Apps/mtroamd";
                After = [ "network.target" ];
              };
              Service = {
                Type = "simple";
                ExecStart = "${cfg.package}/bin/mtroamd serve --addr ${cfg.addr} ${tcpArg}--socket ${cfg.socket}";
                Restart = "on-failure";
                RestartSec = 5;
                KillMode = "process";
              };
              Install.WantedBy = [ "default.target" ];
            };
            warnings = [
              ''
                services.mtroamd is enabled via home-manager. To make it survive
                logout + reboot, set `users.users.<you>.linger = true` in your
                NixOS configuration (home-manager cannot enable linger itself).
              ''
            ];
          };
        };
    in
    perSystem // {
      nixosModules.default = nixosModule;
      homeManagerModules.default = hmModule;
      overlays.default = final: _prev: {
        mtroamd = self.packages.${final.stdenv.hostPlatform.system}.default;
      };
    };
}
