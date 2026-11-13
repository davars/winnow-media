# Security

This library exists to process untrusted media files. What it guarantees
about hostile input, and what it leaves to the deployment, is stated in the
package documentation:
<https://pkg.go.dev/github.com/davars/winnow-media/media#hdr-Hostile_input>.

## Reporting a vulnerability

Report it privately through GitHub: the repository's **Security** tab,
**Report a vulnerability**. Do not open a public issue.

In scope is anything that breaks one of those guarantees, for example:

- an input that gets decoded by a coder or demuxer other than the one its
  type and `Config.Types` select;
- an input that makes ImageMagick or ffmpeg open another file, a delegate or
  the network;
- a tool that can create a socket, gain privileges, or exceed the limits the
  sandbox reports as applied;
- a process that outlives its call;
- an input admitted past `Config.Caps` or the fixed limits.

A memory-safety bug in a decoder itself (ImageMagick, ffmpeg, libwebp, libheif
and the other libraries they use) is not something the library claims to
prevent. Report it upstream. Report it here too if the library's
configuration should have kept that decoder from running on the input.

Fixes are made on the latest release.
