# Publishing the device client to npm

The public package is `codex-broker`. Its npm name must be available when the
first publish runs; checking availability does not reserve it.

## One-time setup

1. Create an account at https://www.npmjs.com/signup, verify your email, and
   enable two-factor authentication. Keep passwords, tokens, and recovery codes
   out of chat, source files, and workflow logs.
2. Download the **usage-sync-npm** artifact from a successful branch CI run in
   https://github.com/NotRllyRn/codex-broker/actions and extract its `.tgz` file.
   This contains all six native builds; you do not need Go or a local build.
3. In your own terminal with Node.js 24 and current npm installed, run:

   ```sh
   npm login
   npm publish ./codex-broker-VERSION.tgz --access public
   ```

   Replace the filename with the downloaded tarball. Complete npm's browser/2FA
   prompts. This first publish creates the public package under your account.
4. Open the package's **Settings → Trusted publishing** on npmjs.com. Select
   **GitHub Actions** and enter:

   - Organization/user: `NotRllyRn`
   - Repository: `codex-broker`
   - Workflow filename: `ci.yml`
   - Environment: leave empty (the workflow does not use an environment).

5. After the changes land on main, run **CI → Run workflow → main**, or push a
   main-branch change. Verify the npm publish job succeeds and test
   `npx codex-broker@latest version` from another terminal.

GitHub-hosted CI uses npm trusted publishing/OIDC and provenance. No `NPM_TOKEN`
secret is needed. If publishing fails, check the exact owner/repository/workflow
settings and npm trusted-publishing support before adding token credentials.
The first main build will fail its publish step until setup is completed; its
built tarball remains available as an artifact. Retrying a workflow increments
its version, avoiding an attempt to overwrite an immutable npm package version.

## Routine updates

Successful CI runs on main publish version `0.<run number>.<run attempt>` and
update npm's `latest` tag. PRs and feature branches only upload artifacts. This
intentionally publishes every successful main build to avoid a separate release
process or version bookkeeping. GitHub Releases and tags are not required.

Users rerun `npx codex-broker@latest install` to upgrade. For a known older version,
replace `latest` with its exact version; check collector/protocol compatibility
before recommending a downgrade. Never delete device state to perform an upgrade.
