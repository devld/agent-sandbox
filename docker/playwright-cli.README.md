# Playwright CLI configuration

`cli.config.json` is partly managed by the agent-sandbox startup process. It is
a JSON file; there is no managed `cli.config.js`.

Do not manually modify these fields because they will be overwritten:

- `browser.browserName`
- `browser.launchOptions.chromiumSandbox`
- `browser.launchOptions.executablePath`
- `browser.cdpEndpoint`

The CloakBrowser installer manages the browser name, sandbox setting, and
executable path. On every container startup, `browser.cdpEndpoint` is set to the
supervised Chrome endpoint when `CHROME_REMOTE_DEBUGGING_ENABLED=true` and
removed when remote debugging is disabled.

Other Playwright CLI configuration fields may be customized and are preserved
by these managed updates.
