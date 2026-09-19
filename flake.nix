{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };
  outputs =
    { self, nixpkgs }:
    let
      forAllSystems = nixpkgs.lib.genAttrs [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          codex-broker = pkgs.buildGoModule {
            pname = "codex-broker";
            version = "0.1.0";

            src = ./.;

            vendorHash = "sha256-WRQDFmcrjs0np4ZxWwFQbTNT9KT4Ck0sJLoCSjGF0q8=";

            subPackages = [ "cmd/codex-broker" ];

            meta = {
              description = "Central Codex authentication, quota, and account-routing service";
              homepage = "https://github.com/NotRllyRn/codex-broker";
              mainProgram = "codex-broker";
            };
          };

          default = self.packages.${system}.codex-broker;
        }
      );

      apps = forAllSystems (system: {
        default = {
          type = "app";
          program = "${self.packages.${system}.codex-broker}/bin/codex-broker";
        };
      });

      # NixOS module: `services.codex-broker`.
      nixosModules.default = self.nixosModules.codex-broker;
      nixosModules.codex-broker =
        {
          config,
          lib,
          pkgs,
          ...
        }:
        let
          cfg = config.services.codex-broker;

          # Every tunable maps to a WINDOWKEEPER_* environment variable, matching
          # internal/config/config.go. Secrets are handled separately via systemd
          # credentials, not plain environment variables.
          boolToStr = b: if b then "true" else "false";

          settingsEnv = lib.filterAttrs (_: v: v != null) {
            WINDOWKEEPER_DATA_DIR = cfg.dataDir;
            WINDOWKEEPER_RUNTIME_DIR = cfg.runtimeDir;
            WINDOWKEEPER_LOG_DIR = cfg.logDir;
            WINDOWKEEPER_HOST = cfg.host;
            WINDOWKEEPER_PORT = toString cfg.port;
            WINDOWKEEPER_ROOT_PATH = cfg.rootPath;
            WINDOWKEEPER_TRUSTED_PROXIES =
              if cfg.trustedProxies == [ ] then null else lib.concatStringsSep "," cfg.trustedProxies;

            WINDOWKEEPER_TLS_CERT_FILE = cfg.tls.certFile;
            WINDOWKEEPER_TLS_KEY_FILE = cfg.tls.keyFile;

            WINDOWKEEPER_COOKIE_SECURE = cfg.cookieSecure;
            WINDOWKEEPER_SESSION_IDLE_MINUTES = toString cfg.session.idleMinutes;
            WINDOWKEEPER_SESSION_ABSOLUTE_HOURS = toString cfg.session.absoluteHours;

            WINDOWKEEPER_USAGE_POLL_SECONDS = toString cfg.usage.pollSeconds;
            WINDOWKEEPER_USAGE_REFRESH_CONCURRENCY = toString cfg.usage.refreshConcurrency;

            WINDOWKEEPER_WINDOW_PULSE_ENABLED = boolToStr cfg.windowPulse.enable;
            WINDOWKEEPER_WINDOW_PULSE_POLL_SECONDS = toString cfg.windowPulse.pollSeconds;
            WINDOWKEEPER_WINDOW_PULSE_RETRY_SECONDS = toString cfg.windowPulse.retrySeconds;
            WINDOWKEEPER_WINDOW_PULSE_CONCURRENCY = toString cfg.windowPulse.concurrency;

            WINDOWKEEPER_AUTH_CONCURRENCY = toString cfg.authConcurrency;
            WINDOWKEEPER_PROCESS_START_CONCURRENCY = toString cfg.processStartConcurrency;
            WINDOWKEEPER_RESET_PADDING_SECONDS = toString cfg.resetPaddingSeconds;

            WINDOWKEEPER_BROWSER_OAUTH_MODE = cfg.browserOAuth.mode;
            WINDOWKEEPER_BROWSER_OAUTH_CALLBACK_PORTS =
              if cfg.browserOAuth.callbackPorts == [ ] then
                null
              else
                lib.concatMapStringsSep "," toString cfg.browserOAuth.callbackPorts;
            WINDOWKEEPER_LOGIN_TIMEOUT_SECONDS = toString cfg.browserOAuth.loginTimeoutSeconds;
            WINDOWKEEPER_BROWSER_CALLBACK_MAX_BYTES = toString cfg.browserOAuth.callbackMaxBytes;

            WINDOWKEEPER_CODEX_EXECUTABLE = lib.getExe cfg.codex;
            WINDOWKEEPER_CODEX_VERSION = cfg.codex.version;
            WINDOWKEEPER_LOG_LEVEL = cfg.logLevel;

            # Public enrollment listener (see docs/public-enrollment.md).
            WINDOWKEEPER_PUBLIC_ENROLLMENT_BROKER_URL = cfg.publicEnrollment.brokerUrl;
            WINDOWKEEPER_PUBLIC_ENROLLMENT_CA_CERT = cfg.publicEnrollment.caCert;
            WINDOWKEEPER_PUBLIC_ENROLLMENT_HOST = cfg.publicEnrollment.host;
            WINDOWKEEPER_PUBLIC_ENROLLMENT_PORT = toString cfg.publicEnrollment.port;
            WINDOWKEEPER_PUBLIC_ENROLLMENT_TLS_CERT_FILE = cfg.publicEnrollment.tls.certFile;
            WINDOWKEEPER_PUBLIC_ENROLLMENT_TLS_KEY_FILE = cfg.publicEnrollment.tls.keyFile;
            WINDOWKEEPER_PUBLIC_ENROLLMENT_MAX_ACTIVE = toString cfg.publicEnrollment.maxActive;
            WINDOWKEEPER_PUBLIC_ENROLLMENT_ATTEMPTS_PER_HOUR =
              toString cfg.publicEnrollment.attemptsPerHour;
          };

          # LoadCredential entries: systemd stages each file in $CREDENTIALS_DIRECTORY
          # as a 0400 root-readable-by-service regular file, which satisfies the
          # broker's ReadProtected() check (regular file, no group/other bits).
          credentials = lib.filter (x: x != null) [
            (lib.mapNullable (f: "vault-key:${f}") cfg.vaultKeyFile)
            (lib.mapNullable (f: "admin-password:${f}") cfg.adminPasswordFile)
            (lib.mapNullable (f: "public-enrollment-key:${f}") cfg.publicEnrollment.keyFile)
          ];

          # Point the broker's *_FILE variables at the staged credentials.
          credentialEnv = lib.filterAttrs (_: v: v != null) {
            WINDOWKEEPER_VAULT_KEY_FILE =
              lib.mapNullable (_: "%d/vault-key") cfg.vaultKeyFile;
            WINDOWKEEPER_ADMIN_PASSWORD_FILE =
              lib.mapNullable (_: "%d/admin-password") cfg.adminPasswordFile;
            WINDOWKEEPER_PUBLIC_ENROLLMENT_KEY_FILE =
              lib.mapNullable (_: "%d/public-enrollment-key") cfg.publicEnrollment.keyFile;
          };
        in
        {
          options.services.codex-broker = {
            enable = lib.mkEnableOption "the Codex Broker account-pooling service";

            package = lib.mkOption {
              type = lib.types.package;
              default = self.packages.${pkgs.stdenv.hostPlatform.system}.codex-broker;
              defaultText = lib.literalExpression "codex-broker.packages.\${system}.codex-broker";
              description = "The codex-broker package to run.";
            };

            user = lib.mkOption {
              type = lib.types.str;
              default = "codex-broker";
              description = "User account under which the broker runs. Created if it does not exist.";
            };

            group = lib.mkOption {
              type = lib.types.str;
              default = "codex-broker";
              description = "Group under which the broker runs. Created if it does not exist.";
            };

            dataDir = lib.mkOption {
              type = lib.types.path;
              default = "/var/lib/codex-broker";
              description = ''
                Persistent state directory (SQLite database, vault state, logs).
                Managed as a systemd StateDirectory. Must differ from {option}`runtimeDir`.
                Maps to WINDOWKEEPER_DATA_DIR.
              '';
            };

            runtimeDir = lib.mkOption {
              type = lib.types.path;
              default = "/run/codex-broker";
              description = ''
                Ephemeral runtime directory for per-account codex process state.
                Managed as a systemd RuntimeDirectory. Must differ from {option}`dataDir`.
                Maps to WINDOWKEEPER_RUNTIME_DIR.
              '';
            };

            logDir = lib.mkOption {
              type = lib.types.nullOr lib.types.path;
              default = null;
              description = ''
                Directory for redacted operational logs. When null the broker uses
                `<dataDir>/logs`. Maps to WINDOWKEEPER_LOG_DIR.
              '';
            };

            host = lib.mkOption {
              type = lib.types.str;
              default = "0.0.0.0";
              example = "127.0.0.1";
              description = ''
                Bind address for the authenticated broker listener. Non-loopback
                binds require {option}`tls.certFile` and {option}`tls.keyFile`.
                Maps to WINDOWKEEPER_HOST.
              '';
            };

            port = lib.mkOption {
              type = lib.types.port;
              default = 8787;
              description = "Port for the authenticated broker listener. Maps to WINDOWKEEPER_PORT.";
            };

            rootPath = lib.mkOption {
              type = lib.types.str;
              default = "";
              example = "/broker";
              description = ''
                Optional URL prefix to mount the broker under (for reverse proxies).
                Must start with `/` and must not end with `/`. Maps to WINDOWKEEPER_ROOT_PATH.
              '';
            };

            trustedProxies = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [ ];
              example = [ "10.0.0.0/8" "192.168.1.1" ];
              description = ''
                IP addresses or CIDR ranges of trusted reverse proxies whose
                forwarded-for headers are honored. A `*` wildcard is rejected.
                Maps to WINDOWKEEPER_TRUSTED_PROXIES.
              '';
            };

            tls = {
              certFile = lib.mkOption {
                type = lib.types.nullOr lib.types.path;
                default = null;
                description = "TLS certificate file. Required for non-loopback binds. Maps to WINDOWKEEPER_TLS_CERT_FILE.";
              };
              keyFile = lib.mkOption {
                type = lib.types.nullOr lib.types.path;
                default = null;
                description = "TLS private key file. Required for non-loopback binds. Maps to WINDOWKEEPER_TLS_KEY_FILE.";
              };
            };

            vaultKeyFile = lib.mkOption {
              type = lib.types.nullOr lib.types.path;
              default = null;
              example = "/run/secrets/codex-broker-vault.key";
              description = ''
                Path to a file containing the base64 vault key (generate with
                `codex-broker vault generate-key`). Loaded as a systemd credential,
                so the file only needs to be readable by root at start time.
                Maps to WINDOWKEEPER_VAULT_KEY_FILE.
              '';
            };

            adminPasswordFile = lib.mkOption {
              type = lib.types.nullOr lib.types.path;
              default = null;
              example = "/run/secrets/codex-broker-admin-password";
              description = ''
                Path to a file containing the administrator password. On start the
                broker bootstraps the admin account with this value if it is unset.
                Loaded as a systemd credential. Maps to WINDOWKEEPER_ADMIN_PASSWORD_FILE.
              '';
            };

            cookieSecure = lib.mkOption {
              type = lib.types.enum [ "auto" "true" "false" ];
              default = "auto";
              description = ''
                Whether to set the Secure flag on session cookies. `auto` decides
                based on the request scheme. Maps to WINDOWKEEPER_COOKIE_SECURE.
              '';
            };

            session = {
              idleMinutes = lib.mkOption {
                type = lib.types.ints.positive;
                default = 44640;
                description = "Admin session idle timeout in minutes. Maps to WINDOWKEEPER_SESSION_IDLE_MINUTES.";
              };
              absoluteHours = lib.mkOption {
                type = lib.types.ints.positive;
                default = 2160;
                description = "Admin session absolute lifetime in hours. Maps to WINDOWKEEPER_SESSION_ABSOLUTE_HOURS.";
              };
            };

            usage = {
              pollSeconds = lib.mkOption {
                type = lib.types.ints.positive;
                default = 300;
                description = "Interval (>=60s) between usage-limit polls. Maps to WINDOWKEEPER_USAGE_POLL_SECONDS.";
              };
              refreshConcurrency = lib.mkOption {
                type = lib.types.ints.between 1 16;
                default = 4;
                description = "Concurrent usage refresh workers (1-16). Maps to WINDOWKEEPER_USAGE_REFRESH_CONCURRENCY.";
              };
            };

            windowPulse = {
              enable = lib.mkOption {
                type = lib.types.bool;
                default = true;
                description = "Enable the background window-pulse worker. Maps to WINDOWKEEPER_WINDOW_PULSE_ENABLED.";
              };
              pollSeconds = lib.mkOption {
                type = lib.types.ints.positive;
                default = 60;
                description = "Window-pulse poll interval (>=10s). Maps to WINDOWKEEPER_WINDOW_PULSE_POLL_SECONDS.";
              };
              retrySeconds = lib.mkOption {
                type = lib.types.ints.positive;
                default = 900;
                description = "Window-pulse retry interval (>=60s). Maps to WINDOWKEEPER_WINDOW_PULSE_RETRY_SECONDS.";
              };
              concurrency = lib.mkOption {
                type = lib.types.ints.between 1 8;
                default = 2;
                description = "Window-pulse worker concurrency (1-8). Maps to WINDOWKEEPER_WINDOW_PULSE_CONCURRENCY.";
              };
            };

            authConcurrency = lib.mkOption {
              type = lib.types.ints.between 1 8;
              default = 2;
              description = "Concurrent authentication workers (1-8). Maps to WINDOWKEEPER_AUTH_CONCURRENCY.";
            };

            processStartConcurrency = lib.mkOption {
              type = lib.types.ints.between 1 8;
              default = 2;
              description = "Concurrent codex process starts (1-8). Maps to WINDOWKEEPER_PROCESS_START_CONCURRENCY.";
            };

            resetPaddingSeconds = lib.mkOption {
              type = lib.types.ints.between 0 300;
              default = 10;
              description = "Padding added when computing quota reset times (0-300s). Maps to WINDOWKEEPER_RESET_PADDING_SECONDS.";
            };

            browserOAuth = {
              mode = lib.mkOption {
                type = lib.types.enum [ "disabled" "manual" "host-loopback" ];
                default = "manual";
                description = "Browser OAuth login mode. Maps to WINDOWKEEPER_BROWSER_OAUTH_MODE.";
              };
              callbackPorts = lib.mkOption {
                type = lib.types.listOf (lib.types.enum [ 1455 1457 ]);
                default = [ 1455 1457 ];
                description = ''
                  OAuth callback ports. Only the pinned compatibility values 1455
                  and 1457 are accepted. Maps to WINDOWKEEPER_BROWSER_OAUTH_CALLBACK_PORTS.
                '';
              };
              loginTimeoutSeconds = lib.mkOption {
                type = lib.types.ints.between 60 3600;
                default = 900;
                description = "OAuth login timeout (60-3600s). Maps to WINDOWKEEPER_LOGIN_TIMEOUT_SECONDS.";
              };
              callbackMaxBytes = lib.mkOption {
                type = lib.types.ints.between 1024 65536;
                default = 16384;
                description = "Maximum OAuth callback body size in bytes (1024-65536). Maps to WINDOWKEEPER_BROWSER_CALLBACK_MAX_BYTES.";
              };
            };

            codex = lib.mkOption {
              type = lib.types.package;
              default = pkgs.codex;
              defaultText = lib.literalExpression "pkgs.codex";
              example = lib.literalExpression "pkgs.codex";
              description = ''
                The managed `codex` package. Its `bin/codex` executable maps to
                WINDOWKEEPER_CODEX_EXECUTABLE and its version maps to
                WINDOWKEEPER_CODEX_VERSION.
              '';
            };

            logLevel = lib.mkOption {
              type = lib.types.enum [ "DEBUG" "INFO" "WARN" "ERROR" ];
              default = "INFO";
              description = "Log verbosity. Maps to WINDOWKEEPER_LOG_LEVEL.";
            };

            openFirewall = lib.mkOption {
              type = lib.types.bool;
              default = false;
              description = "Open the broker (and enrolled public) port(s) in the firewall.";
            };

            publicEnrollment = {
              enable = lib.mkOption {
                type = lib.types.bool;
                default = false;
                description = ''
                  Run the isolated Internet-facing device-code enrollment listener
                  (`codex-broker public-serve`) as a second systemd service.
                  See docs/public-enrollment.md.
                '';
              };
              keyFile = lib.mkOption {
                type = lib.types.nullOr lib.types.path;
                default = null;
                description = ''
                  File containing the shared public-enrollment key (>=32 chars).
                  Required when {option}`publicEnrollment.enable` is set. Loaded as a
                  systemd credential. Maps to WINDOWKEEPER_PUBLIC_ENROLLMENT_KEY_FILE.
                '';
              };
              brokerUrl = lib.mkOption {
                type = lib.types.str;
                default = "https://codex-broker:8787";
                description = "HTTPS origin of the internal broker. Maps to WINDOWKEEPER_PUBLIC_ENROLLMENT_BROKER_URL.";
              };
              caCert = lib.mkOption {
                type = lib.types.nullOr lib.types.path;
                default = null;
                description = "Broker CA certificate the enrollment listener trusts. Maps to WINDOWKEEPER_PUBLIC_ENROLLMENT_CA_CERT.";
              };
              host = lib.mkOption {
                type = lib.types.str;
                default = "127.0.0.1";
                description = "Bind address for the public enrollment listener. Maps to WINDOWKEEPER_PUBLIC_ENROLLMENT_HOST.";
              };
              port = lib.mkOption {
                type = lib.types.port;
                default = 8788;
                description = "Port for the public enrollment listener. Maps to WINDOWKEEPER_PUBLIC_ENROLLMENT_PORT.";
              };
              tls = {
                certFile = lib.mkOption {
                  type = lib.types.nullOr lib.types.path;
                  default = null;
                  description = "TLS certificate for the public listener (required for non-loopback binds). Maps to WINDOWKEEPER_PUBLIC_ENROLLMENT_TLS_CERT_FILE.";
                };
                keyFile = lib.mkOption {
                  type = lib.types.nullOr lib.types.path;
                  default = null;
                  description = "TLS private key for the public listener. Maps to WINDOWKEEPER_PUBLIC_ENROLLMENT_TLS_KEY_FILE.";
                };
              };
              maxActive = lib.mkOption {
                type = lib.types.ints.between 1 32;
                default = 4;
                description = "Maximum concurrent enrollment attempts (1-32). Maps to WINDOWKEEPER_PUBLIC_ENROLLMENT_MAX_ACTIVE.";
              };
              attemptsPerHour = lib.mkOption {
                type = lib.types.ints.between 1 20;
                default = 3;
                description = "Enrollment attempts allowed per hour (1-20). Maps to WINDOWKEEPER_PUBLIC_ENROLLMENT_ATTEMPTS_PER_HOUR.";
              };
            };
          };

          config = lib.mkIf cfg.enable {
            assertions = [
              {
                assertion = cfg.dataDir != cfg.runtimeDir;
                message = "services.codex-broker: dataDir and runtimeDir must differ.";
              }
              {
                assertion =
                  cfg.host == "127.0.0.1"
                  || cfg.host == "::1"
                  || cfg.host == "localhost"
                  || (cfg.tls.certFile != null && cfg.tls.keyFile != null);
                message = "services.codex-broker: a non-loopback host requires tls.certFile and tls.keyFile.";
              }
              {
                assertion = !cfg.publicEnrollment.enable || cfg.publicEnrollment.keyFile != null;
                message = "services.codex-broker: publicEnrollment.enable requires publicEnrollment.keyFile.";
              }
              {
                assertion = !cfg.publicEnrollment.enable || cfg.publicEnrollment.caCert != null;
                message = "services.codex-broker: publicEnrollment.enable requires publicEnrollment.caCert.";
              }
            ];

            users.users = lib.mkIf (cfg.user == "codex-broker") {
              codex-broker = {
                isSystemUser = true;
                group = cfg.group;
                home = cfg.dataDir;
              };
            };
            users.groups = lib.mkIf (cfg.group == "codex-broker") {
              codex-broker = { };
            };

            networking.firewall = lib.mkIf cfg.openFirewall {
              allowedTCPPorts =
                [ cfg.port ] ++ lib.optional cfg.publicEnrollment.enable cfg.publicEnrollment.port;
            };

            systemd.services.codex-broker = {
              description = "Codex Broker";
              wantedBy = [ "multi-user.target" ];
              after = [ "network-online.target" ];
              wants = [ "network-online.target" ];

              environment = settingsEnv // credentialEnv;

              serviceConfig = {
                ExecStart = "${lib.getExe cfg.package} serve";
                User = cfg.user;
                Group = cfg.group;
                Restart = "on-failure";
                RestartSec = "5s";

                # dataDir persists as StateDirectory; runtimeDir is ephemeral.
                StateDirectory = lib.mkIf (cfg.dataDir == "/var/lib/codex-broker") "codex-broker";
                RuntimeDirectory = lib.mkIf (cfg.runtimeDir == "/run/codex-broker") "codex-broker";
                RuntimeDirectoryPreserve = "yes";

                LoadCredential = credentials;

                # Hardening.
                NoNewPrivileges = true;
                ProtectSystem = "strict";
                ProtectHome = true;
                PrivateTmp = true;
                PrivateDevices = true;
                ProtectKernelTunables = true;
                ProtectKernelModules = true;
                ProtectControlGroups = true;
                RestrictNamespaces = true;
                RestrictRealtime = true;
                RestrictSUIDSGID = true;
                LockPersonality = true;
                MemoryDenyWriteExecute = false; # Go runtime needs W^X exemptions.
                SystemCallFilter = [ "@system-service" "~@privileged" ];
                ReadWritePaths = [ cfg.dataDir cfg.runtimeDir ] ++ lib.optional (cfg.logDir != null) cfg.logDir;
                CapabilityBoundingSet = "";
                AmbientCapabilities = "";
              };
            };

            systemd.services.codex-broker-public = lib.mkIf cfg.publicEnrollment.enable {
              description = "Codex Broker public enrollment listener";
              wantedBy = [ "multi-user.target" ];
              after = [ "codex-broker.service" "network-online.target" ];
              wants = [ "network-online.target" ];

              environment = settingsEnv // credentialEnv;

              serviceConfig = {
                ExecStart = "${lib.getExe cfg.package} public-serve";
                User = cfg.user;
                Group = cfg.group;
                Restart = "on-failure";
                RestartSec = "5s";

                LoadCredential = credentials;

                NoNewPrivileges = true;
                ProtectSystem = "strict";
                ProtectHome = true;
                PrivateTmp = true;
                PrivateDevices = true;
                ProtectKernelTunables = true;
                ProtectKernelModules = true;
                ProtectControlGroups = true;
                RestrictNamespaces = true;
                RestrictRealtime = true;
                RestrictSUIDSGID = true;
                LockPersonality = true;
                SystemCallFilter = [ "@system-service" "~@privileged" ];
                CapabilityBoundingSet = "";
                AmbientCapabilities = "";
              };
            };
          };
        };
    };
}
