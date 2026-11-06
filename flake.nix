{
  description = "winnow-media - media processing library and its pinned tool closure";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.11";
  };

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      toolSystems = systems ++ [ "x86_64-darwin" "aarch64-darwin" ];

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

      modulePath = "github.com/davars/winnow-media";

      # The commit suffix of a pseudo-version, in any of Go's three forms
      # (vX.0.0-<ts>-<sha>, vX.Y.Z-pre.0.<ts>-<sha>, vX.Y.Z-0.<ts>-<sha>).
      pseudoSuffix = v:
        let m = builtins.match "v[0-9]+\\.[0-9]+\\.[0-9]+-([0-9A-Za-z.-]*[.-])?[0-9]{14}-([0-9a-f]{12})(\\+incompatible)?" v;
        in if m == null then null else builtins.elemAt m 1;

      # Returns true when go.mod's requirement of this module names the
      # commit `rev`; throws otherwise, so a mismatch fails evaluation.
      checkGoModPin = { goModText, rev }:
        let
          lines = nixpkgs.lib.splitString "\n" goModText;
          reqOf = line:
            builtins.match "[[:space:]]*(require[[:space:]]+)?${nixpkgs.lib.escapeRegex modulePath}[[:space:]]+([^[:space:]]+).*" line;
          isReplace = line:
            builtins.match ".*${nixpkgs.lib.escapeRegex modulePath}([[:space:]].*)?=>.*" line != null;
          reqs = builtins.filter (m: m != null) (map reqOf lines);
          version = builtins.elemAt (builtins.head reqs) 1;
          suffix = pseudoSuffix version;
          want = builtins.substring 0 12 rev;
        in
        if builtins.any isReplace lines then
          throw "winnow-media: go.mod has a replace directive for ${modulePath}; the pin cannot be checked"
        else if reqs == [ ] then
          throw "winnow-media: go.mod has no requirement for ${modulePath}"
        else if suffix == null then
          throw "winnow-media: go.mod requires ${modulePath} ${version}, which is not a pseudo-version; only pseudo-versions are supported"
        else if suffix != want then
          throw "winnow-media: go.mod pins ${modulePath} at ${suffix} but flake.lock locks ${want}; move go.mod and flake.lock together"
        else true;

      # Uses this flake's own revision, i.e. the one the consumer locked.
      assertGoModPin = goModPath:
        if self ? rev then
          checkGoModPin { goModText = builtins.readFile goModPath; rev = self.rev; }
        else
          builtins.trace "winnow-media: no revision for this input (dirty or path override); go.mod pin NOT checked" true;

      pinFixtures =
        let
          run = f:
            let r = builtins.tryEval (checkGoModPin { goModText = f.text; inherit rev; });
            in f // { actual = r.success && r.value == true; };
          rev = "104cc9a7f5f4aaaabbbbccccddddeeeeffff0000";
          good = "v0.0.0-20261103230040-104cc9a7f5f4";
        in
        map run [
          { name = "block form"; want = true; text = "module x\n\nrequire (\n\tgithub.com/other/y v1.0.0\n\t${modulePath} ${good}\n)\n"; }
          { name = "block form, indirect"; want = true; text = "require (\n\t${modulePath} ${good} // indirect\n)\n"; }
          { name = "single-line form"; want = true; text = "module x\n\nrequire ${modulePath} ${good}\n"; }
          { name = "pre-release pseudo-version"; want = true; text = "require ${modulePath} v1.2.3-pre.0.20261103230040-104cc9a7f5f4\n"; }
          { name = "next-patch pseudo-version"; want = true; text = "require ${modulePath} v1.2.4-0.20261103230040-104cc9a7f5f4\n"; }
          { name = "mismatched hash"; want = false; text = "require ${modulePath} v0.0.0-20261103230040-aaaaaaaaaaaa\n"; }
          { name = "missing requirement"; want = false; text = "module x\n\nrequire github.com/other/y v1.0.0\n"; }
          { name = "tag version"; want = false; text = "require ${modulePath} v1.2.3\n"; }
          { name = "replace directive"; want = false; text = "require ${modulePath} ${good}\n\nreplace ${modulePath} => ../winnow-media\n"; }
          { name = "replace directive in block"; want = false; text = "require ${modulePath} ${good}\n\nreplace (\n\t${modulePath} => ../winnow-media\n)\n"; }
        ];

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

          # The measurement harness runs the Processor, which resolves
          # magick, ffmpeg and ffprobe, and generates inputs with the same tools.
          measureTools = [ "imagemagick" "ffmpeg" ];
          measure = buildGoModule {
            pname = "measure";
            version = "0";
            inherit src;
            vendorHash = null;
            subPackages = [ "internal/cmd/measure" "internal/cmd/measure4d" ];
            ldflags = ldflags { inherit system; tools = measureTools; };
            meta.mainProgram = "measure";
          };

          measureImage = pkgs.dockerTools.buildLayeredImage {
            name = "winnow-media-measure";
            tag = "latest";
            contents = [ measure pkgs.busybox ];
            extraCommands = "mkdir -m 1777 tmp";
            config.Entrypoint = [ "${measure}/bin/measure" ];
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
          inherit pkgs tools allTools golangci-lint buildGoModule gate closureCheck measure measureImage;
        };
    in
    {
      packages = nixpkgs.lib.genAttrs toolSystems (system:
        toolPackages nixpkgs.legacyPackages.${system} // {
          default = toolDir { inherit system; tools = toolNames; };
        } // nixpkgs.lib.optionalAttrs (builtins.elem system systems) {
          measure = (perSystem system).measure;
          measure-image = (perSystem system).measureImage;
        });

      apps = nixpkgs.lib.genAttrs systems (system: {
        measure = {
          type = "app";
          program = "${(perSystem system).measure}/bin/measure";
        };
      });

      lib = { inherit toolDir ldflags checkGoModPin assertGoModPin; };

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
          pin =
            let
              rows = map (f: "${if f.want == f.actual then "ok  " else "FAIL"} ${f.name}: want ${builtins.toJSON f.want}, got ${builtins.toJSON f.actual}") pinFixtures;
              report = nixpkgs.lib.concatStringsSep "\n" rows;
            in
            if builtins.all (f: f.want == f.actual) pinFixtures
            then s.pkgs.writeText "winnow-media-pin-check" report
            else throw "winnow-media: pin check fixtures failed:\n${report}";
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
