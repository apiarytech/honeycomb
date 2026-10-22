# Contributing to honeycomb

honeycomb is an IEC 61131-3 tag database. Bug reports, feature requests, code, tests and
documentation are all welcome.

## Issues and feature requests

- Search the open issues before filing a new one.
- Use the issue templates: a bug report wants the version, the platform
  and the steps; a feature request wants the use case.
- A security problem is not an issue. See [SECURITY.md](SECURITY.md) for
  the private channel.
- Do not post credentials, customer data, plant tag names or anything else
  private. The issue history is public and permanent.

**Terms of submission.** Everything posted to this repository's issues,
pull requests and discussions is public. By posting, you give the Project
Owner (the copyright holder named in [LICENSE.md](LICENSE.md)) the right
to use what you post, including ideas and feature suggestions, in any
way: in the open-source and the commercial editions of honeycomb and in
other products, with no obligation or payment to you. Code you submit is
covered by the contributor licence agreement below.

## Code

1. **Open an issue first** for anything beyond a small fix, so the change
   can be discussed before you spend time on it.
2. **Sign the CLA.** honeycomb is dual-licensed (open source or commercial,
   see LICENSE.md), so every contributor signs the
   [contributor licence agreement](CLA.md) once. The check on your first
   pull request tells you how; it takes one comment. Pull requests are
   not merged until the author has signed.
3. **Fork, branch, change.** Run `gofmt`, `go vet ./...` and the tests:

   ```sh
   go test ./...
   ```
   If the change touches the PLC4X connector, run its tests too:
   `cd connectors/plc4x && go test ./...`.

4. **Keep dependencies minimal.** Do not add a module to `go.mod` without
   discussing it in the issue first.
5. **Commit messages**: one imperative subject line, a body that says why.
6. **Pull request**: link the issue, describe the change, and make sure
   the GitHub Actions checks pass.

## Licence of your contribution

Your contribution is licensed under the same terms as the rest of honeycomb
(the GNU General Public License version 3 (GPLv3) or the commercial licence) and, through the CLA, to the Project
Owner so that both licences remain available.
