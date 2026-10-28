{
  description = "factory: provision Raspberry Pi hosts that run dagu";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "aarch64-linux" "x86_64-linux" ];
      forAll = f: nixpkgs.lib.genAttrs systems (s: f nixpkgs.legacyPackages.${s});
    in {
      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.ansible
            pkgs.ansible-lint
            pkgs.yamllint
            pkgs.go
            pkgs.golangci-lint
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
