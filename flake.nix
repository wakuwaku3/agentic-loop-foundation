{
  description = "Reproducible development environment for agentic-loop-foundation";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs = { nixpkgs, ... }:
    let
      supportedSystems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = nixpkgs.lib.genAttrs supportedSystems;
    in {
      devShells = forAllSystems (system:
        let pkgs = nixpkgs.legacyPackages.${system};
        in {
          default = pkgs.mkShellNoCC {
            packages = with pkgs; [
              bash
              coreutils
              findutils
              gawk
              git
              gnugrep
              gnumake
              gnused
              shellcheck
              util-linux
            ];
            DEV_ENVIRONMENT = "agentic-loop-foundation-v1";
          };
        });
    };
}
