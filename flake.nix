{
  description = "winnow-media - media processing library and its pinned tool closure";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.11";
  };

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];

      toolNames = [ "exiftool" "file" "ffmpeg" "imagemagick" "poppler-utils" "pandoc" "catdoc" "lynx" ];

      toolPackages = pkgs: nixpkgs.lib.genAttrs toolNames (name: pkgs.${name});

      # Unknown names fail evaluation rather than being skipped.
      toolDir = { system, tools }:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          all = toolPackages pkgs;
          pick = name:
            all.${name} or (throw "winnow-media: unknown tool '${name}'; known tools: ${nixpkgs.lib.concatStringsSep ", " toolNames}");
        in
        # Flat: <dir>/<program>, not <dir>/bin/<program>. A name two tools
        # both provide fails the build instead of picking one.
        pkgs.runCommand "winnow-media-tools" { } ''
          mkdir -p $out
          ${nixpkgs.lib.concatMapStringsSep "\n" (p: ''
            for f in ${nixpkgs.lib.getBin p}/bin/*; do
              ln -s "$f" "$out/$(basename "$f")"
            done
          '') (map pick tools)}
        '';

      ldflags = { system, tools }: [
        "-X" "github.com/davars/winnow-media/tool.toolDir=${toolDir { inherit system tools; }}"
        "-X" "github.com/davars/winnow-media/tool.flakeRev=${self.rev or self.dirtyRev or "unknown"}"
      ];

      perSystem = system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          lib = pkgs.lib;

          buildGoModule = pkgs.buildGoModule.override { go = pkgs.go_1_26; };

          # golangci-lint pins buildGo125Module; rebuild it against go_1_26 so
          # it accepts this module's go directive.
          golangci-lint = pkgs.golangci-lint.override { buildGo125Module = buildGoModule; };

          src = lib.cleanSource ./.;

          tools = toolPackages pkgs;
          allTools = builtins.attrValues tools;

          # A Go-only gate: no dependencies to vendor, output is an empty file.
          gate = name: nativeBuildInputs: script: buildGoModule {
            pname = "winnow-media-${name}";
            version = "0";
            inherit src nativeBuildInputs;
            vendorHash = null;
            buildPhase = ''
              runHook preBuild
              export HOME="$TMPDIR/home"
              export GOCACHE="$TMPDIR/go-cache"
              export GOLANGCI_LINT_CACHE="$TMPDIR/golangci-lint-cache"
              mkdir -p "$HOME"
              ${script}
              runHook postBuild
            '';
            doCheck = false;
            installPhase = "touch $out";
          };

          # Built the way a consumer builds a binary: lib.ldflags and
          # nixpkgs' default buildGoModule flags, nothing else.
          probeWith = probeLdflags: buildGoModule {
            pname = "toolprobe";
            version = "0";
            inherit src;
            vendorHash = null;
            subPackages = [ "internal/cmd/toolprobe" ];
            ldflags = probeLdflags;
          };

          probeTools = [ "imagemagick" ];
          probeDir = toolDir { inherit system; tools = probeTools; };
          probe = probeWith (ldflags { inherit system; tools = probeTools; });

          closure = pkgs.closureInfo { rootPaths = [ probe ]; };

          closureCheck = pkgs.runCommand "winnow-media-closure-check" { } ''
            got=$(${probe}/bin/toolprobe | head -n1)
            want=${probeDir}/magick
            if [ "$got" != "$want" ]; then
              echo "toolprobe resolved '$got', want '$want'" >&2
              exit 1
            fi
            "$got" -version >/dev/null

            if ! grep -qxF ${pkgs.imagemagick} ${closure}/store-paths; then
              echo "imagemagick ${pkgs.imagemagick} missing from the probe's runtime closure" >&2
              exit 1
            fi
            touch $out
          '';
        in
        {
          inherit pkgs tools allTools golangci-lint buildGoModule gate closureCheck;
        };
    in
    {
      packages = nixpkgs.lib.genAttrs systems (system:
        let s = perSystem system; in
        s.tools // {
          default = toolDir { inherit system; tools = toolNames; };
        });

      lib = { inherit toolDir ldflags; };

      checks = nixpkgs.lib.genAttrs systems (system:
        let s = perSystem system; in
        {
          test = s.gate "test" s.allTools "go test ./...";
          lint = s.gate "lint" [ s.golangci-lint ] "golangci-lint run ./...";
          gofumpt = s.gate "gofumpt" [ s.pkgs.gofumpt ] ''
            unformatted=$(gofumpt -l .)
            if [ -n "$unformatted" ]; then
              echo "gofumpt would reformat:" >&2
              echo "$unformatted" >&2
              exit 1
            fi
          '';
          closure = s.closureCheck;
        });

      devShells = nixpkgs.lib.genAttrs systems (system:
        let s = perSystem system; pkgs = s.pkgs; in
        {
          default = pkgs.mkShell {
            packages = [ pkgs.go_1_26 s.golangci-lint pkgs.gofumpt pkgs.gopls ] ++ s.allTools;
          };
        });
    };
}
