# nixpkgs pinned to one revision, so the Go and just here are the same for
# everyone who enters the shell.
let
  nixpkgs = builtins.fetchTarball {
    url = "https://github.com/NixOS/nixpkgs/archive/e554fab72f81915600f3f449b786fd9af40439a5.tar.gz";
    sha256 = "sha256-ZKhUe/2IJUq1JhKxKMu8rbkgSGmPP2ZCqlIPn40aGCM=";
  };
in
{
  pkgs ? import nixpkgs { },
}:

pkgs.mkShell {
  name = "go-sdk-notify-order";
  packages = with pkgs; [
    go
    just
    git
  ];

  shellHook = ''
    # Use the Go this shell pins, and fail rather than fetch another.
    export GOTOOLCHAIN=local
  '';
}
