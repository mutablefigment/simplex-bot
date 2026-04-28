{ lib, buildGoModule }:

buildGoModule {
  pname = "claude-bot";
  version = "0.1.0";

  src = lib.cleanSource ../.;

  # First build will fail with the real hash; paste it here.
  vendorHash = lib.fakeHash;

  ldflags = [ "-s" "-w" "-X main.version=0.1.0" ];

  meta = with lib; {
    description = "Single-user SimpleX <-> Claude Code bridge bot";
    license = licenses.mit;
    mainProgram = "claude-bot";
  };
}
