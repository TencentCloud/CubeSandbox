// SPDX-License-Identifier: Apache-2.0
// Copyright (C) 2026 Tencent. All rights reserved.
// playwright-cli evaluates this whole file as a parenthesized function.
// Keep the final function expression free of a trailing semicolon.

async (page) => {
  const configStorageKey = "cube.e2e.webTerminalLifecycle";
  const configText = await page.evaluate(
    (key) => sessionStorage.getItem(key),
    configStorageKey,
  );
  await page.evaluate(
    (key) => sessionStorage.removeItem(key),
    configStorageKey,
  );
  if (!configText) {
    throw new Error("Missing Web Terminal lifecycle test configuration");
  }

  const config = JSON.parse(configText);
  const baseURL = String(config.baseURL).replace(/\/+$/, "");
  const baseOrigin = await page.evaluate((url) => new URL(url).origin, baseURL);
  const actionTimeoutMs = Number(config.actionTimeoutMs);
  const lifecycleTimeoutMs = Number(config.lifecycleTimeoutMs);
  const requestTimeoutMs = Math.min(actionTimeoutMs, 10_000);
  const cleanupStateTimeoutMs = Math.min(lifecycleTimeoutMs, 15_000);
  let sandboxID = "";
  let selectedTemplateID = "";

  page.setDefaultTimeout(actionTimeoutMs);
  page.setDefaultNavigationTimeout(actionTimeoutMs);

  const sleep = (milliseconds) => page.waitForTimeout(milliseconds);

  const isVisible = async (locator) => {
    try {
      return await locator.isVisible();
    } catch {
      return false;
    }
  };

  const waitForEnabled = async (locator, label, timeout = actionTimeoutMs) => {
    await locator.waitFor({ state: "visible", timeout });
    const deadline = Date.now() + timeout;
    while (Date.now() < deadline) {
      if (await locator.isEnabled()) return;
      await sleep(250);
    }
    const title = await locator.getAttribute("title").catch(() => "");
    throw new Error(
      `${label} did not become enabled${title ? `: ${title}` : ""}`,
    );
  };

  const waitForTerminalText = async (expected, timeout = actionTimeoutMs) => {
    const deadline = Date.now() + timeout;
    while (Date.now() < deadline) {
      const text = await page
        .locator(".xterm-rows:visible, .xterm-accessibility-tree:visible")
        .allInnerTexts()
        .then((values) => values.join("\n"))
        .catch(() => "");
      if (text.includes(expected)) return;
      await sleep(200);
    }
    throw new Error(
      `Terminal output did not contain ${JSON.stringify(expected)}`,
    );
  };

  const runTerminalCommand = async (command, expected) => {
    const input = page.locator(".xterm-helper-textarea:visible").last();
    await input.waitFor({ state: "visible" });
    await input.focus();
    await page.keyboard.insertText(command);
    await page.keyboard.press("Enter");
    await waitForTerminalText(expected);
  };

  const waitForConnected = async (dialog) => {
    await dialog
      .getByText(/^(Connected|已连接)$/)
      .last()
      .waitFor({ state: "visible", timeout: actionTimeoutMs });
  };

  const requestSandbox = async (method, path, body) =>
    page.evaluate(
      async ({
        method: requestMethod,
        path: requestPath,
        body: requestBody,
        timeoutMs,
      }) => {
        const token = localStorage.getItem("cube.accessToken") ?? "";
        const controller = new AbortController();
        const timer = setTimeout(() => controller.abort(), timeoutMs);
        try {
          const response = await fetch(requestPath, {
            method: requestMethod,
            headers: {
              ...(token ? { Authorization: `Bearer ${token}` } : {}),
              ...(requestBody === undefined
                ? {}
                : { "Content-Type": "application/json" }),
            },
            body:
              requestBody === undefined
                ? undefined
                : JSON.stringify(requestBody),
            signal: controller.signal,
          });
          const text = await response.text();
          let data = null;
          if (text) {
            try {
              data = JSON.parse(text);
            } catch {
              data = text;
            }
          }
          return { ok: response.ok, status: response.status, data };
        } catch (error) {
          return {
            ok: false,
            status: 0,
            data: error instanceof Error ? error.message : String(error),
          };
        } finally {
          clearTimeout(timer);
        }
      },
      { method, path, body, timeoutMs: requestTimeoutMs },
    );

  const waitForSandboxState = async (
    expected,
    timeout = lifecycleTimeoutMs,
  ) => {
    const deadline = Date.now() + timeout;
    let latest = null;
    while (Date.now() < deadline) {
      latest = await requestSandbox("GET", `/sandboxes/${sandboxID}`);
      if (latest.status === 404) return expected === "deleted";
      if (latest.ok && latest.data?.state === expected) return true;
      await sleep(1000);
    }
    throw new Error(
      `Sandbox ${sandboxID} did not reach ${expected}; last response: ${JSON.stringify(latest)}`,
    );
  };

  const cleanupSandbox = async () => {
    if (!sandboxID) return "no sandbox was created";

    const closeTerminalButton = page.getByRole("button", {
      name: /^(Close terminal|关闭终端)$/,
    });
    if (await isVisible(closeTerminalButton)) {
      await closeTerminalButton.click().catch(() => {});
      await sleep(250);
    }

    for (let attempt = 1; attempt <= 2; attempt += 1) {
      const detail = await requestSandbox("GET", `/sandboxes/${sandboxID}`);
      if (detail.status === 404) return "sandbox already deleted";

      if (detail.ok && detail.data?.state !== "running") {
        await requestSandbox("POST", `/sandboxes/${sandboxID}/resume`, {
          timeout: 15,
          autoPause: false,
        });
        await waitForSandboxState("running", cleanupStateTimeoutMs).catch(
          () => false,
        );
      }

      const removed = await requestSandbox("DELETE", `/sandboxes/${sandboxID}`);
      if (removed.ok || removed.status === 404) {
        await waitForSandboxState("deleted", cleanupStateTimeoutMs).catch(
          () => false,
        );
        return "sandbox deleted";
      }
      await sleep(5000);
    }

    throw new Error(
      `failed to delete sandbox ${sandboxID} after two cleanup attempts`,
    );
  };

  try {
    await page.goto(`${baseURL}/login`, { waitUntil: "domcontentloaded" });

    const usernameInput = page.locator("#login-username");
    if (await isVisible(usernameInput)) {
      await usernameInput.fill(config.username);
      await page.locator("#login-password").fill(config.password);
      await page.locator('form button[type="submit"]').click();
      await page.waitForURL(
        (url) => url.origin === baseOrigin && url.pathname !== "/login",
        { timeout: actionTimeoutMs },
      );
    }

    await page.goto(`${baseURL}/sandboxes/new`, {
      waitUntil: "domcontentloaded",
    });
    await page.waitForTimeout(1000);

    let templateButton;
    if (config.templateID) {
      templateButton = page
        .locator('button[type="button"]:not([disabled])')
        .filter({ hasText: config.templateID })
        .first();
      selectedTemplateID = config.templateID;
    } else {
      templateButton = page
        .locator('button[type="button"]:not([disabled])')
        .filter({ hasText: /READY/i })
        .first();
    }
    await templateButton.waitFor({
      state: "visible",
      timeout: actionTimeoutMs,
    });
    if (!selectedTemplateID) {
      selectedTemplateID = (await templateButton.innerText()).split(/\s+/)[0];
    }
    await templateButton.click();

    const createButton = page.getByRole("button", {
      name: /^(Create sandbox|创建沙箱)$/,
    });
    await waitForEnabled(createButton, "Create sandbox");
    await createButton.click();
    await page.waitForURL(
      (url) =>
        /^\/sandboxes\/[^/]+$/.test(url.pathname) &&
        !url.pathname.endsWith("/new"),
      { timeout: lifecycleTimeoutMs },
    );
    sandboxID = await page.evaluate(
      () => location.pathname.split("/").filter(Boolean).at(-1) ?? "",
    );
    if (!sandboxID)
      throw new Error("Could not determine the created sandbox ID");

    const terminalButton = page.getByRole("button", {
      name: /^(Open Terminal|打开终端)$/,
    });
    await waitForEnabled(
      terminalButton,
      `Open Terminal for template ${selectedTemplateID}; set E2E_TEMPLATE_ID to an envd-enabled READY template if auto-selection chose the wrong template`,
      lifecycleTimeoutMs,
    );
    await terminalButton.click();

    const dialog = page.getByRole("dialog");
    await dialog.waitFor({ state: "visible" });
    await waitForConnected(dialog);
    await runTerminalCommand(
      "command -v ls top ping >/dev/null && ls --color=always / | head -n 3 && top -b -n1 | head -n 3 && ping -c1 127.0.0.1 && printf 'CUBE_E2E_BASIC_TOOLS_OK\\n'",
      "CUBE_E2E_BASIC_TOOLS_OK",
    );
    await runTerminalCommand(
      "printf 'CUBE_E2E_COMMAND_OK\\n'; export CUBE_E2E_TAB=one",
      "CUBE_E2E_COMMAND_OK",
    );

    await dialog
      .getByRole("button", { name: /^(New terminal session|新建终端会话)$/ })
      .click();
    const tabs = dialog.getByRole("tab");
    const tabDeadline = Date.now() + actionTimeoutMs;
    while ((await tabs.count()) !== 2 && Date.now() < tabDeadline) {
      await sleep(100);
    }
    if ((await tabs.count()) !== 2)
      throw new Error("Second terminal tab was not created");
    await waitForConnected(dialog);
    await runTerminalCommand(
      "printf 'CUBE_E2E_TAB2_OK:%s\\n' \"${CUBE_E2E_TAB-unset}\"",
      "CUBE_E2E_TAB2_OK:unset",
    );

    await tabs.nth(0).click();
    await runTerminalCommand(
      "printf 'CUBE_E2E_TAB1_STATE:%s\\n' \"$CUBE_E2E_TAB\"",
      "CUBE_E2E_TAB1_STATE:one",
    );

    await dialog
      .getByRole("button", { name: /^(Close Shell 2|关闭 Shell 2)$/ })
      .click();
    if ((await tabs.count()) !== 1)
      throw new Error("Second terminal tab did not close");

    await dialog
      .getByRole("button", { name: /^(Close terminal|关闭终端)$/ })
      .click();
    await dialog.waitFor({ state: "hidden" });

    const pauseButton = page.getByRole("button", { name: /^(Pause|暂停)$/ });
    const resumeButton = page.getByRole("button", { name: /^(Resume|恢复)$/ });
    const killButton = page.getByRole("button", { name: /^(Kill|终止)$/ });

    await waitForEnabled(pauseButton, "Pause after closing terminal");
    await pauseButton.click();
    await resumeButton.waitFor({
      state: "visible",
      timeout: lifecycleTimeoutMs,
    });

    await waitForEnabled(resumeButton, "Resume");
    await resumeButton.click();
    await waitForEnabled(pauseButton, "Pause after resume", lifecycleTimeoutMs);

    await pauseButton.click();
    await resumeButton.waitFor({
      state: "visible",
      timeout: lifecycleTimeoutMs,
    });

    await waitForEnabled(killButton, "Kill while paused");
    await killButton.click();
    const deleteDeadline = Date.now() + lifecycleTimeoutMs;
    while (Date.now() < deleteDeadline) {
      const pathname = await page.evaluate(() => location.pathname);
      if (/\/sandboxes\/?$/.test(pathname)) break;
      const alert = page.getByRole("alert");
      if (await isVisible(alert)) {
        throw new Error(
          `Paused sandbox deletion failed: ${await alert.innerText()}`,
        );
      }
      await sleep(250);
    }
    const pathnameAfterDelete = await page.evaluate(() => location.pathname);
    if (!/\/sandboxes\/?$/.test(pathnameAfterDelete)) {
      throw new Error(
        "Paused sandbox deletion did not return to the sandbox list",
      );
    }

    const detailAfterDelete = await requestSandbox(
      "GET",
      `/sandboxes/${sandboxID}`,
    );
    if (detailAfterDelete.status !== 404) {
      throw new Error(
        `Sandbox ${sandboxID} still exists after Kill: ${JSON.stringify(detailAfterDelete)}`,
      );
    }

    await page.screenshot({
      path: `${config.artifactDir}/passed.png`,
      fullPage: true,
    });
    return {
      status: "passed",
      sandboxID,
      templateID: selectedTemplateID,
      workflow: [
        "create",
        "terminal command",
        "two isolated tabs",
        "close terminal",
        "pause",
        "resume",
        "pause",
        "delete",
      ],
    };
  } catch (error) {
    await page
      .screenshot({
        path: `${config.artifactDir}/failed.png`,
        fullPage: true,
      })
      .catch(() => {});
    let cleanupResult = "";
    try {
      cleanupResult = await cleanupSandbox();
    } catch (cleanupError) {
      cleanupResult = `cleanup failed: ${
        cleanupError instanceof Error
          ? cleanupError.message
          : String(cleanupError)
      }`;
    }
    const reason = error instanceof Error ? error.message : String(error);
    throw new Error(`${reason}; cleanup: ${cleanupResult}`);
  }
}
