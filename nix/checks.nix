{
  self,
  nixpkgs,
  system,
}:
let
  pkgs = nixpkgs.legacyPackages.${system};
  lib = nixpkgs.lib;
  # Evaluation fixture only; no fake Codex binary is built or executed.
  codex = pkgs.runCommand "codex-evaluation-fixture" {
    version = "0.145.0";
    meta.mainProgram = "codex";
  } "mkdir -p $out";
  evaluate =
    settings:
    (lib.nixosSystem {
      inherit system;
      modules = [
        self.nixosModules.default
        {
          system.stateVersion = "26.05";
          services.codex-broker = {
            enable = true;
            inherit codex;
          }
          // settings;
        }
      ];
    }).config;
  basic = evaluate { };
  custom = evaluate {
    dataDir = "/srv/broker/data";
    runtimeDir = "/run/broker-custom";
    logDir = "/srv/broker/logs";
    vaultKeyFile = "/run/secrets/vault";
    adminPasswordFile = "/run/secrets/admin-credential";
    publicEnrollment = {
      enable = true;
      keyFile = "/run/secrets/enrollment";
      caCert = "/etc/broker/ca.crt";
      tls.certFile = "/run/secrets/public-cert";
      tls.keyFile = "/run/secrets/public-key";
    };
  };
  broker = custom.systemd.services.codex-broker;
  public = custom.systemd.services.codex-broker-public;
  valid =
    config:
    builtins.all (
      a: a.assertion || !(lib.hasPrefix "services.codex-broker:" a.message)
    ) config.assertions;
in
assert valid basic;
assert valid custom;
assert
  !valid (evaluate {
    codex = pkgs.codex;
  });
assert
  !valid (evaluate {
    host = "0.0.0.0";
  });
assert
  !valid (evaluate {
    publicEnrollment.enable = true;
  });
assert
  !valid (evaluate {
    dataDir = "/run/codex-broker";
  });
assert basic.systemd.services.codex-broker.environment.WINDOWKEEPER_HOST == "127.0.0.1";
assert basic.systemd.services.codex-broker.serviceConfig.StateDirectoryMode == "0700";
assert builtins.elem "d /srv/broker/data 0700 codex-broker codex-broker -"
  custom.systemd.tmpfiles.rules;
assert builtins.elem "d /run/broker-custom 0700 codex-broker codex-broker -"
  custom.systemd.tmpfiles.rules;
assert builtins.elem "d /srv/broker/logs 0700 codex-broker codex-broker -"
  custom.systemd.tmpfiles.rules;
assert broker.environment.WINDOWKEEPER_VAULT_KEY_FILE == "%d/vault-key";
assert public.environment.WINDOWKEEPER_PUBLIC_ENROLLMENT_KEY_FILE == "%d/public-enrollment-key";
assert !(public.environment ? WINDOWKEEPER_VAULT_KEY_FILE);
assert !(public.environment ? WINDOWKEEPER_ADMIN_PASSWORD_FILE);
assert public.serviceConfig.DynamicUser;
assert
  public.serviceConfig.LoadCredential == [
    "public-enrollment-key:/run/secrets/enrollment"
    "public-tls-cert:/run/secrets/public-cert"
    "public-tls-key:/run/secrets/public-key"
  ];
pkgs.runCommand "codex-broker-module-check" { } "touch $out"
