#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
"""Browser layout regression using synthetic API responses, never a cluster.

Requires the optional Python playwright package and a running local Vite server.
See web/README.md for commands. No application CSS or DOM is altered by the test.
"""

import argparse
import json
from pathlib import Path
from urllib.parse import urlsplit

from playwright.sync_api import expect, sync_playwright

STATUSES = ("FAILED", "PARTIALLY_READY", "READY", "RUNNING", "READY")
TEMPLATES = [
    {
        "templateID": "tpl-" + f"{index + 1:024x}",
        "aliases": ["openclaw-template-with-a-very-long-unbroken-alias-" * 3] if index == 2 else [],
        "status": status,
        "instanceType": "cubebox",
        "version": "v2",
        "imageInfo": "registry.example.invalid/demo/openclaw:latest",
        "jobID": f"{index + 1:08x}-0000-4000-8000-000000000000",
        "createdAt": "2026-09-25T00:00:00Z",
    }
    for index, status in enumerate(STATUSES)
]
COMPAT = {
    "summary": {
        "staleTemplates": 0,
        "staleReplicas": 0,
        "affectedNodes": 0,
        "missingReplicas": 0,
        "unknownReplicas": 1,
    },
    "templates": [{"templateID": TEMPLATES[-1]["templateID"], "overall": "UNKNOWN", "nodes": []}],
}


def overlaps(left, right):
    return (
        min(left["x"] + left["width"], right["x"] + right["width"]) > max(left["x"], right["x"]) + 1
        and min(left["y"] + left["height"], right["y"] + right["height"])
        > max(left["y"], right["y"]) + 1
    )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default="http://127.0.0.1:5173")
    parser.add_argument("--browser-executable")
    parser.add_argument(
        "--output", type=Path, help="Optional directory for Light screenshots and geometry results"
    )
    parser.add_argument(
        "--baseline",
        action="store_true",
        help="Capture an unfixed checkout; require a reproduced collision",
    )
    args = parser.parse_args()
    origin = urlsplit(args.url)
    if origin.scheme != "http" or origin.hostname not in {"127.0.0.1", "localhost", "::1"}:
        parser.error("only a local test server is allowed")
    base = args.url.rstrip("/")
    errors, cases = [], 0
    if args.output:
        args.output.mkdir(parents=True, exist_ok=True)

    def route_request(route):
        request = route.request
        path = urlsplit(request.url).path
        # Vite's API proxy also matches /templates document navigations.
        if request.is_navigation_request() and path == "/templates":
            route.fulfill(response=route.fetch(url=base + "/index.html"))
        elif request.resource_type in {"fetch", "xhr"}:
            if request.method != "GET":
                raise AssertionError("layout tests must not mutate a backend")
            value = (
                TEMPLATES
                if path == "/templates"
                else COMPAT
                if path == "/templates/compat"
                else {"authRequired": False, "authenticated": False}
                if path.endswith("/auth/session")
                else {"components": [], "nodes": [], "config": {}}
            )
            route.fulfill(json=value)
        else:
            route.continue_()

    with sync_playwright() as pw:
        browser = pw.chromium.launch(headless=True, executable_path=args.browser_executable)
        for language in ("en", "zh"):
            context = browser.new_context(
                viewport={"width": 1440, "height": 1100}, color_scheme="light", locale=language
            )
            context.route("**/*", route_request)
            context.add_init_script(
                "localStorage.setItem('cube-theme','light'); localStorage.setItem('cube.lang',"
                + json.dumps(language)
                + ");"
            )
            page = context.new_page()
            page.goto(base + "/templates")
            expect(
                page.locator('a[href="/templates/' + TEMPLATES[0]["templateID"] + '"]')
            ).to_be_visible()
            page.evaluate("document.fonts.ready")
            for width in (360, 768, 1024, 1280, 1440):
                page.set_viewport_size({"width": width, "height": 1100})
                for template in TEMPLATES:
                    link = page.locator('a[href="/templates/' + template["templateID"] + '"]')
                    wrapper = link.locator("..")
                    card = link.locator(".panel")
                    header = card.locator(":scope > div").first
                    badge = header.locator("span").last
                    button = wrapper.locator("button")
                    wrapper.hover()
                    page.wait_for_timeout(180)  # opacity transition, not data loading
                    bounds, status, action = (
                        card.bounding_box(),
                        badge.bounding_box(),
                        button.bounding_box(),
                    )
                    tag = {
                        "language": language,
                        "width": width,
                        "status": template["status"],
                        "id": template["templateID"],
                    }
                    for label, box in (("status", status), ("delete", action)):
                        if (
                            box["x"] < bounds["x"] - 1
                            or box["x"] + box["width"] > bounds["x"] + bounds["width"] + 1
                            or box["y"] < bounds["y"] - 1
                            or box["y"] + box["height"] > bounds["y"] + bounds["height"] + 1
                        ):
                            errors.append({**tag, "error": label + " outside card"})
                    if overlaps(status, action):
                        errors.append({**tag, "error": "status/delete overlap"})
                    if overlaps(header.locator("h3").bounding_box(), status):
                        errors.append({**tag, "error": "title/status overlap"})
                    if badge.evaluate("el => el.scrollWidth > el.clientWidth + 1"):
                        errors.append({**tag, "error": "status clipped"})
                    expect(header.locator("h3")).to_have_attribute(
                        "title",
                        template["aliases"][0] if template["aliases"] else template["templateID"],
                    )
                    expect(header.locator("p")).to_have_attribute("title", template["templateID"])
                    cases += 1
                if width == 1440 and args.output:
                    first = page.locator('a[href="/templates/' + TEMPLATES[0]["templateID"] + '"]')
                    first.locator("..").hover()
                    page.wait_for_timeout(180)
                    grid = first.locator("../..")
                    grid.screenshot(
                        path=str(args.output / ("template-cards-" + language + "-light.png"))
                    )
            if not args.baseline:
                # Hidden hover actions must also remain visible to keyboard users.
                ready = page.locator('a[href="/templates/' + TEMPLATES[2]["templateID"] + '"]')
                page.mouse.move(0, 0)
                ready.focus()
                page.keyboard.press("Tab")
                button = ready.locator("..").locator("button")
                expect(button).to_be_focused()
                page.wait_for_timeout(180)
                assert button.evaluate("el => getComputedStyle(el).opacity") == "1"
            # Opening the confirmation must not navigate or issue a DELETE.
            first = page.locator('a[href="/templates/' + TEMPLATES[0]["templateID"] + '"]')
            first.locator("..").locator("button").click()
            expect(page).to_have_url(base + "/templates")
            expect(
                page.get_by_role(
                    "heading", name="Delete Template" if language == "en" else "删除模板"
                )
            ).to_be_visible()
            context.close()
        browser.close()
    report = {
        "cases": cases,
        "errors": errors,
        "fixture_responses": True,
        "color_scheme": "light",
        "baseline": args.baseline,
    }
    if args.output:
        (args.output / "results.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))
    if args.baseline:
        assert errors, "expected the original layout regression to reproduce"
    else:
        assert not errors, "template card geometry regressed"


if __name__ == "__main__":
    main()
