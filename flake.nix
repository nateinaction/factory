{
  description = "factory: provision Raspberry Pi hosts that run dagu";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "aarch64-linux" "x86_64-linux" ];
      forAll = f: nixpkgs.lib.genAttrs systems (s: f nixpkgs.legacyPackages.${s});
    in {
      # foreman is a static Go binary, so hosts need no Go toolchain.
      packages = forAll (pkgs: rec {
        foreman = pkgs.buildGo127Module {
          pname = "foreman";
          version = "0.1.0";
          src = ./foreman;
          # Update after any go.mod change: set lib.fakeHash, build, and copy the "got" hash.
          vendorHash = "sha256-S38KonlL5c8nyd+U0KjeQAc8vz6+00nuVf1UMsjDHtM=";
          env.CGO_ENABLED = "0";
          subPackages = [ "cmd/foreman" ];
          meta.mainProgram = "foreman";
        };
        default = foreman;
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.ansible
            pkgs.ansible-lint
            pkgs.yamllint
            pkgs.go_1_27
            pkgs.golangci-lint
            pkgs.govulncheck
          ];
          # Install the pre-commit hook by pointing git at the tracked hooks.
          shellHook = ''
            if git rev-parse --git-dir >/dev/null 2>&1; then
              git config core.hooksPath scripts/hooks
            fi
          '';
        };
      });
    };
}
