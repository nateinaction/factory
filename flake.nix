{
  description = "factory: provision Raspberry Pi hosts that run dagu";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "aarch64-linux" "x86_64-linux" ];
      forAll = f: nixpkgs.lib.genAttrs systems (s: f nixpkgs.legacyPackages.${s});

      # The sandbox backend: @anthropic-ai/sandbox-runtime. Pinned to the exact version the worker
      # hosts install (factory_srt_version in group_vars), so the dev shell and a host agree.
      srtVersion = "0.0.81";

      # vercel-labs/emulate is a Node package and not in nixpkgs. Pin the npm
      # version and its lock file, so the tests' GitHub emulator is reproducible.
      emulate = pkgs: pkgs.buildNpmPackage {
        pname = "emulate";
        version = "0.12.3";
        src = ./nix/emulate;
        npmDepsHash = "sha256-5nKi0JGlWjH65JfrgMruJ9zb55kv25YWjywkZQcuRjE=";
        dontNpmBuild = true;
        nativeBuildInputs = [ pkgs.makeWrapper ];
        postInstall = ''
          makeWrapper ${pkgs.nodejs_22}/bin/node $out/bin/emulate \
            --add-flags "$out/lib/node_modules/emulate-nix/node_modules/emulate/dist/index.js"
        '';
        meta.mainProgram = "emulate";
      };
    in {
      # foreman is a static Go binary, so hosts need no Go toolchain.
      packages = forAll (pkgs: rec {
        foreman = pkgs.buildGo127Module {
          pname = "foreman";
          version = "0.1.0";
          src = ./foreman;
          # Update after any go.mod change: set lib.fakeHash, build, and copy the "got" hash.
          vendorHash = "sha256-ia3QMa95zY1LRjOQk+YjgGKjoc+6SV9f7i767Ycf+as=";
          env.CGO_ENABLED = "0";
          subPackages = [ "cmd/foreman" ];
          meta.mainProgram = "foreman";
        };
        default = foreman;
      });

      devShells = forAll (pkgs: assert pkgs.lib.assertMsg
        (pkgs.sandbox-runtime.version == srtVersion)
        "nixpkgs provides sandbox-runtime ${pkgs.sandbox-runtime.version}, but the factory pins ${srtVersion}";
      {
        default = pkgs.mkShell {
          packages = [
            pkgs.ansible
            pkgs.ansible-lint
            pkgs.yamllint
            # promtool checks and unit-tests the Prometheus alert rules tests/alerts.test renders. It is
            # the package's `cli` output; `out` carries the server binary the host installs.
            pkgs.prometheus.cli
            pkgs.go_1_27
            pkgs.gitleaks
            pkgs.golangci-lint
            pkgs.govulncheck
            # The sandbox backend every model run is wrapped in; also runs the acceptance test.
            pkgs.sandbox-runtime
            # The pinned collector the hosts install, so the collector role's rendered config is
            # validated and exercised by the real binary.
            pkgs.opentelemetry-collector-contrib
            # Pinned to 4.7.0 by the locked nixpkgs revision (see flake.lock).
            pkgs.pre-commit
            # foreman's tests run the GitHub emulator for their state.
            (emulate pkgs)
          ];
          # Install the git hook from .pre-commit-config.yaml. A leftover hooksPath
          # would take precedence over the hook pre-commit writes, so clear it.
          shellHook = ''
            if git rev-parse --git-dir >/dev/null 2>&1; then
              git config --unset-all core.hooksPath || true
              pre-commit install
            fi
          '';
        };
      });
    };
}
