{
  description = "lard — memory layer for homelab LLM sessions";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
  };

  outputs =
    {
      self,
      nixpkgs,
    }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
        "x86_64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (system: {
        default =
          let
            pkgs = nixpkgs.legacyPackages.${system};
          in
          pkgs.buildGoModule {
            pname = "lard";
            version = "0.2.0";

            src = ./.;

            vendorHash = "sha256-8n+5kNTK1ZUzRBEli3T/l7WACvOZ2eKBTk0XuE1o7+E=";

            subPackages = [ "cmd/lard" ];

            meta = with pkgs.lib; {
              description = "Memory layer for homelab LLM sessions";
              homepage = "https://lard.dunkirk.sh";
              license = licenses.mit;
              platforms = platforms.unix;
            };
          };
      });

      apps = forAllSystems (system: {
        default = {
          type = "app";
          program = "${self.packages.${system}.default}/bin/lard";
        };
      });

      devShells = forAllSystems (system: {
        default =
          let
            pkgs = nixpkgs.legacyPackages.${system};
          in
          pkgs.mkShell {
            packages = with pkgs; [
              go
              sqlite
            ];
          };
      });
    };
}
