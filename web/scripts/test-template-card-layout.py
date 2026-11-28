#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
"""Browser layout regression using synthetic API responses, never a cluster.

Requires the optional Python playwright package and a running local Vite server.
See web/README.md for commands. No application CSS or DOM is altered by the test.
"""

import argparse
import json
import time
from pathlib import Path
from types import SimpleNamespace
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
RESPONSES = {
    "/templates": TEMPLATES,
    "/templates/compat": COMPAT,
    "/opsapi/v1/auth/session": {"authRequired": False, "authenticated": False},
    "/opsapi/v1/cluster/versions": {
        "components": [],
        "nodes": [],
        "controlPlane": {"version": "0.7.3"},
    },
}


def route_request(route, base, fixture_errors):
    request = route.request
    path = urlsplit(request.url).path
    # Vite's API proxy also matches /templates document navigations.
    if request.is_navigation_request() and path == "/templates":
        route.fulfill(response=route.fetch(url=base + "/index.html"))
    elif request.resource_type in {"fetch", "xhr"}:
        if request.method != "GET" or path not in RESPONSES:
            fixture_errors.append(
                {"error": "unexpected fixture request", "method": request.method, "path": path}
            )
            route.fulfill(status=500, json={"error": "unexpected layout fixture request"})
        else:
            route.fulfill(json=RESPONSES[path])
    else:
        route.continue_()


def check_fixture_handler():
    cases = [("GET", path, "fetch", 200) for path in RESPONSES]
    cases += [
        ("GET", "/unexpected-endpoint", "fetch", 500),
        ("DELETE", "/templates", "fetch", 500),
        ("GET", "/src/main.tsx", "script", None),
    ]
    for method, path, resource_type, expected in cases:
        replies, continued, fixture_errors = [], [], []
        route = SimpleNamespace(
            request=SimpleNamespace(
                url="http://localhost" + path,
                method=method,
                resource_type=resource_type,
                is_navigation_request=lambda: False,
            ),
            fulfill=lambda replies=replies, **reply: replies.append(reply),
            continue_=lambda continued=continued: continued.append(True),
        )
        route_request(route, "http://localhost", fixture_errors)
        if expected is None:
            assert len(continued) == 1 and not replies and not fixture_errors
        else:
            assert len(replies) == 1, "fixture requests must always be completed"
            assert replies[0].get("status", 200) == expected
            assert bool(fixture_errors) == (expected == 500)
    return len(cases)


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
    fixture_errors, layout_errors = [], []
    cases, wrapped_cases = 0, 0
    started = time.monotonic()
    fixture_checks = check_fixture_handler()
    if args.output:
        args.output.mkdir(parents=True, exist_ok=True)

    with sync_playwright() as pw:
        browser = pw.chromium.launch(headless=True, executable_path=args.browser_executable)
        for language in ("en", "zh"):
            context = browser.new_context(
                viewport={"width": 1440, "height": 1100}, color_scheme="light", locale=language
            )
            context.route("**/*", lambda route: route_request(route, base, fixture_errors))
            context.add_init_script(
                "localStorage.setItem('cube-theme','light'); localStorage.setItem('cube.lang',"
                + json.dumps(language)
                + ");"
            )
            page = context.new_page()
            page.goto(base + "/templates")
            try:
                expect(
                    page.locator('a[href="/templates/' + TEMPLATES[0]["templateID"] + '"]')
                ).to_be_visible()
            except Exception as error:
                if fixture_errors:
                    raise AssertionError(
                        "Unexpected fixture requests: " + json.dumps(fixture_errors)
                    ) from error
                raise
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
                            layout_errors.append({**tag, "error": label + " outside card"})
                    if overlaps(status, action):
                        layout_errors.append({**tag, "error": "status/delete overlap"})
                    if overlaps(header.locator("h3").bounding_box(), status):
                        layout_errors.append({**tag, "error": "title/status overlap"})
                    if badge.evaluate("el => el.scrollWidth > el.clientWidth + 1"):
                        layout_errors.append({**tag, "error": "status clipped"})
                    title = header.locator(":scope > div").bounding_box()
                    wrapped = status["y"] >= title["y"] + title["height"] - 1
                    if wrapped:
                        wrapped_cases += 1
                        if not args.baseline:
                            content_right = header.evaluate(
                                "el => el.getBoundingClientRect().right - parseFloat(getComputedStyle(el).paddingRight)"
                            )
                            if abs(status["x"] + status["width"] - content_right) > 1:
                                layout_errors.append(
                                    {**tag, "error": "wrapped status is not right-aligned"}
                                )
                    if width == 360 and not args.baseline:
                        assert wrapped, "the narrow fixture must exercise a wrapped status"
                    expected_title = (
                        template["aliases"][0] if template["aliases"] else template["templateID"]
                    )
                    expect(header.locator("h3")).to_have_attribute("title", expected_title)
                    expect(header.locator("h3")).to_have_text(expected_title)
                    expect(header.locator("p")).to_have_attribute("title", template["templateID"])
                    expect(header.locator("p")).to_have_text(template["templateID"])
                    cases += 1
                if width in (360, 1440) and args.output:
                    subject_id = TEMPLATES[1 if width == 360 else 0]["templateID"]
                    subject = page.locator('a[href="/templates/' + subject_id + '"]')
                    if width == 360:
                        # Use ordinary page scrolling and a viewport crop: an
                        # element screenshot of the tall grid can scroll it
                        # underneath the dashboard's fixed navigation rail.
                        page.evaluate("window.scrollTo(0, 0)")
                        clip = subject.locator("..").bounding_box()
                        page.evaluate(
                            "y => window.scrollTo(0, y)",
                            max(0, clip["y"] + clip["height"] / 2 - 550),
                        )
                        clip = subject.locator("..").bounding_box()
                        page.mouse.move(
                            clip["x"] + clip["width"] / 2, clip["y"] + clip["height"] / 2
                        )
                    else:
                        subject.locator("..").hover()
                    # Wait for visible state only when pixels matter. Bounding
                    # boxes above are independent of the opacity transition.
                    for template in TEMPLATES:
                        other = page.locator('a[href="/templates/' + template["templateID"] + '"]')
                        opacity = (
                            "1"
                            if template["templateID"] == subject_id
                            or template["status"] == "FAILED"
                            else "0"
                        )
                        expect(other.locator("..").locator("button")).to_have_css(
                            "opacity", opacity
                        )
                    target = str(args.output / (f"template-cards-{language}-{width}-light.png"))
                    if width == 360:
                        page.screenshot(path=target, clip=clip)
                    else:
                        subject.locator("../..").screenshot(path=target)
            delete_label = "Delete Template" if language == "en" else "删除模板"
            if not args.baseline:
                # Hidden hover actions must also remain visible and labelled for keyboard users.
                ready = page.locator('a[href="/templates/' + TEMPLATES[2]["templateID"] + '"]')
                page.mouse.move(0, 0)
                ready.focus()
                page.keyboard.press("Tab")
                button = ready.locator("..").locator("button")
                expect(button).to_be_focused()
                expect(button).to_have_css("opacity", "1")
                expect(button).to_have_attribute("aria-label", delete_label)
            # Opening the confirmation must not navigate or issue a DELETE.
            delete_requests = []
            page.on(
                "request",
                lambda req, seen=delete_requests: (
                    seen.append(req.url) if req.method == "DELETE" else None
                ),
            )
            first = page.locator('a[href="/templates/' + TEMPLATES[0]["templateID"] + '"]')
            first.locator("..").locator("button").click()
            expect(page).to_have_url(base + "/templates")
            expect(page.get_by_role("heading", name=delete_label)).to_be_visible()
            assert not delete_requests, f"opening delete modal issued DELETE: {delete_requests}"
            context.close()
        browser.close()
    report = {
        "cases": cases,
        "fixture_checks": fixture_checks,
        "wrapped_cases": wrapped_cases,
        "elapsed_seconds": round(time.monotonic() - started, 2),
        "fixture_errors": fixture_errors,
        "layout_errors": layout_errors,
        "fixture_responses": True,
        "color_scheme": "light",
        "baseline": args.baseline,
    }
    if args.output:
        (args.output / "results.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))
    assert not fixture_errors, f"unexpected fixture requests: {fixture_errors}"
    assert cases == 50, f"expected 50 cases, got {cases}"
    if args.baseline:
        assert layout_errors, "expected the original layout regression to reproduce"
    else:
        # Pre-fix CardHeader lacked flex-wrap, so wrapping only occurs after the fix.
        assert wrapped_cases > 0, "the narrow fixture must exercise a wrapped status"
        assert not layout_errors, "template card geometry regressed"


if __name__ == "__main__":
    main()
