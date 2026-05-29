{ lib, buildGoModule }:

buildGoModule {
  pname = "claude-bot";
  version = "0.1.0";

  src = lib.cleanSource ../.;

  # Dependencies are vendored in-tree (`go mod vendor`), so no fixed-output
  # hash is needed and the build is reproducible without a manual paste step.
  vendorHash = null;

  ldflags = [ "-s" "-w" "-X main.version=0.1.0" ];

  meta = with lib; {
    description = "Single-user SimpleX <-> Claude Code bridge bot";
    license = licenses.mit;
    mainProgram = "claude-bot";
  };
}
