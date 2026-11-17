{
  description = "winnow-media - media processing library and its pinned tool closure";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
  };

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      toolSystems = systems ++ [ "x86_64-darwin" "aarch64-darwin" ];

      # The tools the flake builds, by name: the package, and the programs
      # toolDir links from it, which are the programs package media runs. The
      # closure check compares them with what internal/cmd/toolprobe, which
      # asks the Go package, says the library can run.
      toolTable = {
        ffmpeg = { package = pkgs: pkgs.ffmpeg-headless; programs = [ "ffmpeg" "ffprobe" ]; };
        imagemagick = { package = pkgs: pkgs.imagemagick; programs = [ "magick" ]; };
        poppler = { package = pkgs: pkgs.poppler-utils; programs = [ "pdfinfo" "pdftoppm" ]; };
        # webp-frames links the pinned libwebp, the build ImageMagick links.
        webp-frames = {
          package = pkgs: pkgs.stdenv.mkDerivation {
            pname = "webp-frames";
            version = "1";
            src = ./tools/webp-frames;
            buildInputs = [ pkgs.libwebp ];
            buildPhase = ''
              runHook preBuild
              $CC -O2 -Wall -Wextra -Werror -o webp-frames webp-frames.c -lwebpmux -lwebpdemux -lwebp
              runHook postBuild
            '';
            installPhase = ''
              runHook preInstall
              install -Dm755 webp-frames $out/bin/webp-frames
              runHook postInstall
            '';
            meta.mainProgram = "webp-frames";
          };
          programs = [ "webp-frames" ];
        };
      };

      toolNames = builtins.attrNames toolTable;

      toolPackages = pkgs: nixpkgs.lib.mapAttrs (_: t: t.package pkgs) toolTable;

      # Programs the tests use to build and inspect inputs; they are not part
      # of the tool closure. cjpeg is in libjpeg-turbo's bin output, setsid in
      # util-linux.
      testToolPackages = pkgs: [ pkgs.libwebp pkgs.exiftool pkgs.libjpeg_turbo.bin pkgs.util-linux ];

      # Unknown names fail evaluation rather than being skipped.
      toolDir = { system, tools }:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          all = toolPackages pkgs;
          pick = name:
            toolTable.${name} or (throw "winnow-media: unknown tool '${name}'; known tools: ${nixpkgs.lib.concatStringsSep ", " toolNames}");
          link = name:
            let
              bin = nixpkgs.lib.getBin all.${name};
              ln = prog: ''
                test -x "${bin}/bin/${prog}" || { echo "winnow-media: ${name} has no ${prog}" >&2; exit 1; }
                ln -s "${bin}/bin/${prog}" "$out/${prog}"
              '';
            in
            nixpkgs.lib.concatMapStringsSep "\n" ln (pick name).programs;
        in
        # Flat: <dir>/<program>, not <dir>/bin/<program>. A name two tools
        # both provide fails the build instead of picking one.
        pkgs.runCommand "winnow-media-tools" { } ''
          mkdir -p $out
          ${nixpkgs.lib.concatMapStringsSep "\n" link tools}
        '';

      # The release version, exported as lib.version. The commit that is
      # tagged vX.Y.Z carries version "X.Y.Z"; the next commit moves it to a
      # "-dev" value, so an untagged commit never matches a tag.
      version = "1.1.1-dev";

      modulePath = "github.com/davars/winnow-media";

      # The commit suffix of a pseudo-version, in any of Go's three forms
      # (vX.0.0-<ts>-<sha>, vX.Y.Z-pre.0.<ts>-<sha>, vX.Y.Z-0.<ts>-<sha>).
      pseudoSuffix = v:
        let m = builtins.match "v[0-9]+\\.[0-9]+\\.[0-9]+-([0-9A-Za-z.-]*[.-])?[0-9]{14}-([0-9a-f]{12})(\\+incompatible)?" v;
        in if m == null then null else builtins.elemAt m 1;

      # Returns true when go.mod's requirement of this module names the
      # commit `rev` (a pseudo-version) or is the tag "v" + version; throws
      # otherwise, so a mismatch fails evaluation.
      checkGoModPin = { goModText, rev }:
        let
          lines = nixpkgs.lib.splitString "\n" goModText;
          reqOf = line:
            builtins.match "[[:space:]]*(require[[:space:]]+)?${nixpkgs.lib.escapeRegex modulePath}[[:space:]]+([^[:space:]]+).*" line;
          isReplace = line:
            builtins.match ".*${nixpkgs.lib.escapeRegex modulePath}([[:space:]].*)?=>.*" line != null;
          reqs = builtins.filter (m: m != null) (map reqOf lines);
          goModVersion = builtins.elemAt (builtins.head reqs) 1;
          suffix = pseudoSuffix goModVersion;
          want = builtins.substring 0 12 rev;
        in
        if builtins.any isReplace lines then
          throw "winnow-media: go.mod has a replace directive for ${modulePath}; the pin cannot be checked"
        else if reqs == [ ] then
          throw "winnow-media: go.mod has no requirement for ${modulePath}"
        else if suffix == null then
          if goModVersion == "v${version}" then true
          else throw "winnow-media: go.mod requires ${modulePath} ${goModVersion}, which is neither a pseudo-version nor this flake's version v${version}; move go.mod and flake.lock together"
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
          { name = "tag equal to the flake version"; want = true; text = "require ${modulePath} v${version}\n"; }
          { name = "tag equal to the flake version, indirect"; want = true; text = "require (\n\t${modulePath} v${version} // indirect\n)\n"; }
          { name = "tag other than the flake version"; want = false; text = "require ${modulePath} v${version}.1\n"; }
          { name = "older tag"; want = false; text = "require ${modulePath} v1.2.3\n"; }
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

          inherit (pkgs) golangci-lint;

          src = lib.cleanSource ./.;

          tools = toolPackages pkgs;
          allTools = builtins.attrValues tools ++ testToolPackages pkgs;

          # The hash of the vendored modules in go.mod and go.sum; every build
          # of this module shares it. Redrive it when they change: set it to
          # lib.fakeHash, build, and paste the hash Nix prints.
          vendorHash = "sha256-3/tNPMYKxKjwprceoPRjRJuTNBwJSJE4LD8grnh+haI=";

          # A Go gate: its output is an empty file.
          gate = name: nativeBuildInputs: script: buildGoModule {
            pname = "winnow-media-${name}";
            version = "0";
            inherit src nativeBuildInputs;
            inherit vendorHash;
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
            inherit vendorHash;
            subPackages = [ "internal/cmd/toolprobe" ];
            ldflags = probeLdflags;
          };

          # The measurement harness runs the Processor, which resolves
          # magick, ffmpeg and ffprobe, and generates inputs with the same tools.
          measureTools = [ "imagemagick" "ffmpeg" "webp-frames" "poppler" ];
          measure = buildGoModule {
            pname = "measure";
            version = "0";
            inherit src;
            inherit vendorHash;
            subPackages = [ "internal/cmd/measure" ];
            ldflags = ldflags { inherit system; tools = measureTools; };
            meta.mainProgram = "measure";
          };

          probeTools = [ "imagemagick" "ffmpeg" "webp-frames" ];
          probeDir = toolDir { inherit system; tools = probeTools; };
          probe = probeWith (ldflags { inherit system; tools = probeTools; });

          closure = pkgs.closureInfo { rootPaths = [ probe ]; };

          # The same, built as a consumer that renders builds it.
          renderTools = [ "imagemagick" "webp-frames" "poppler" ];
          renderDir = toolDir { inherit system; tools = renderTools; };
          renderProbe = probeWith (ldflags { inherit system; tools = renderTools; });
          renderClosure = pkgs.closureInfo { rootPaths = [ renderProbe ]; };

          # How each program is run to see that it starts. webp-frames has no
          # version flag; run bare it prints its usage and exits 1.
          versionFlag = {
            magick = "-version";
            ffmpeg = "-version";
            ffprobe = "-version";
            pdfinfo = "-v";
            pdftoppm = "-v";
          };
          runProgram = prog:
            if prog == "webp-frames"
            then ''[[ "$("$got" 2>&1 || true)" == *usage:* ]] || fail "webp-frames did not print its usage"''
            else ''"$got" ${versionFlag.${prog} or (throw "winnow-media: no version flag for ${prog}")} >/dev/null 2>&1 || fail "${prog} did not run"'';

          # Asserts of a probe binary built with `listed` tools and `dir`: it
          # names exactly the programs the flake knows, each program of a
          # listed tool resolves into dir and runs, and each program of any
          # other tool is absent, as is that tool's package from the binary's
          # runtime closure.
          probeCheck = { name, probe, dir, listed, closure }:
            let
              programsOf = names: lib.concatMap (n: toolTable.${n}.programs) names;
              unlisted = lib.filter (n: !(lib.elem n listed)) toolNames;
              known = lib.concatStringsSep " " (lib.sort lib.lessThan (programsOf toolNames));
              binOf = n: lib.getBin tools.${n};
            in
            ''
              fail() { echo "${name}: $1" >&2; exit 1; }
              report=$(${probe}/bin/toolprobe)
              pathOf() { echo "$report" | awk -v p="$1" '$1 == p { print $2 }'; }
              programs=$(echo "$report" | awk '$1 != "flake-revision" { print $1 }' | sort | xargs)
              [ "$programs" = "${known}" ] || fail "toolprobe names '$programs', the flake knows '${known}'"
              ${lib.concatMapStringsSep "\n" (prog: ''
                got=$(pathOf ${prog})
                [ "$got" = "${dir}/${prog}" ] || fail "${prog} resolved to '$got', want ${dir}/${prog}"
                ${runProgram prog}
              '') (programsOf listed)}
              ${lib.concatMapStringsSep "\n" (prog: ''
                got=$(pathOf ${prog})
                [ -z "$got" ] || fail "${prog} of an unlisted tool resolved to '$got'"
              '') (programsOf unlisted)}
              ${lib.concatMapStringsSep "\n" (n: ''
                grep -qxF ${binOf n} ${closure}/store-paths || fail "${n} is missing from the runtime closure"
              '') listed}
              ${lib.concatMapStringsSep "\n" (n: ''
                ! grep -qxF ${binOf n} ${closure}/store-paths || fail "${n} is in the runtime closure of a build that does not list it"
              '') unlisted}
            '';

          closureCheck = pkgs.runCommand "winnow-media-closure-check" { } ''
            ${probeCheck { name = "without poppler"; inherit probe closure; dir = probeDir; listed = probeTools; }}
            ${probeCheck { name = "with poppler"; probe = renderProbe; closure = renderClosure; dir = renderDir; listed = renderTools; }}
            touch $out
          '';
        in
        {
          inherit pkgs tools allTools golangci-lint buildGoModule gate closureCheck measure;
        };
    in
    {
      packages = nixpkgs.lib.genAttrs toolSystems (system:
        toolPackages nixpkgs.legacyPackages.${system} // {
          default = toolDir { inherit system; tools = toolNames; };
        } // nixpkgs.lib.optionalAttrs (builtins.elem system systems) {
          measure = (perSystem system).measure;
        });

      apps = nixpkgs.lib.genAttrs systems (system: {
        measure = {
          type = "app";
          program = "${(perSystem system).measure}/bin/measure";
        };
      });

      lib = { inherit version toolDir ldflags checkGoModPin assertGoModPin; };

      checks = nixpkgs.lib.genAttrs systems (system:
        let s = perSystem system; in
        {
          test = s.gate "test" s.allTools "go test -timeout 30m ./...";
          lint = s.gate "lint" [ s.golangci-lint ] "golangci-lint run ./...";
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
            # The integration-heavy media suite outlasts go test's 10m default.
            GOFLAGS = "-timeout=30m";
          };
        });
    };
}
