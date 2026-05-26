# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Dict

import requests

from ._config import Config
from ._exceptions import ApiError, AuthenticationError, TemplateNotFoundError


def _check_response(resp: requests.Response) -> None:
    if resp.ok:
        return
    try:
        msg = resp.json().get("message") or resp.json().get("detail") or resp.text
    except Exception:
        msg = resp.text or f"HTTP {resp.status_code}"
    code = resp.status_code
    if code in (401, 403):
        raise AuthenticationError(msg, code)
    if code == 404:
        raise TemplateNotFoundError(msg, code)
    raise ApiError(msg, code)


@dataclass
class TemplateBuild:
    """A single build record for a template."""

    build_id: str
    status: str
    created_at: str = ""
    finished_at: str = ""
    logs: list[str] = field(default_factory=list)

    @classmethod
    def from_dict(cls, data: dict) -> "TemplateBuild":
        return cls(
            build_id=data.get("buildID") or data.get("build_id", ""),
            status=data.get("status", ""),
            created_at=data.get("createdAt") or data.get("created_at", ""),
            finished_at=data.get("finishedAt") or data.get("finished_at", ""),
            logs=data.get("logs") or [],
        )


@dataclass
class TemplateInfo:
    """Metadata for a CubeSandbox template."""

    template_id: str
    name: str = ""
    public: bool = False
    cpu_count: int = 0
    memory_mb: int = 0
    builds: list[TemplateBuild] = field(default_factory=list)

    @classmethod
    def from_dict(cls, data: dict) -> "TemplateInfo":
        builds_raw = data.get("builds") or []
        return cls(
            template_id=data.get("templateID") or data.get("template_id", ""),
            name=data.get("name") or data.get("aliases", [None])[0] or "",
            public=bool(data.get("public", False)),
            cpu_count=data.get("cpuCount") or data.get("cpu_count", 0),
            memory_mb=data.get("memoryMB") or data.get("memory_mb", 0),
            builds=[TemplateBuild.from_dict(b) for b in builds_raw],
        )


class Template:
    """Class-level helper for Cube template management.

    All methods are class-methods / static-methods — no instance required.

    Example::

        # List all templates
        templates = Template.list()
        for t in templates:
            print(t.template_id, t.name)

        # Build a new template from a docker image
        info = Template.build(
            image="python:3.11-slim",
            name="my-python-env",
        )
        print(info.template_id)

        # Query a specific template
        detail = Template.get("tpl-xxxxxxxxxxxxxxxxxxxxxxxx")
        print(detail.builds)

        # Update template metadata
        Template.update("tpl-xxx", name="new-name")

        # Delete a template
        Template.delete("tpl-xxx")
    """


    @classmethod
    def list(cls, *, config: Config | None = None) -> list[TemplateInfo]:
        """GET /templates — List all templates.

        Args:
            config: SDK config.  Uses default (env-based) config if omitted.

        Returns:
            A list of :class:`TemplateInfo` objects.

        Raises:
            ApiError: On unexpected backend error.
        """
        cfg = config or Config()
        s = requests.Session()
        resp = s.get(f"{cfg.api_url}/templates")
        _check_response(resp)
        data = resp.json() or []
        if isinstance(data, dict):
            # Some implementations wrap the list
            data = data.get("templates") or data.get("items") or []
        return [TemplateInfo.from_dict(d) for d in data]


    @classmethod
    def get(
        cls,
        template_id: str,
        *,
        limit: int | None = None,
        next_token: str | None = None,
        config: Config | None = None,
    ) -> TemplateInfo:
        """GET /templates/:templateID — Get a template and its build history.

        Args:
            template_id: Template identifier.
            limit: Maximum number of builds to return.
            next_token: Pagination cursor for builds.
            config: SDK config.  Uses default (env-based) config if omitted.

        Returns:
            :class:`TemplateInfo` with ``builds`` populated.

        Raises:
            TemplateNotFoundError: If the template does not exist (HTTP 404).
            ApiError: On unexpected backend error.
        """
        cfg = config or Config()
        params: dict = {}
        if limit is not None:
            params["limit"] = limit
        if next_token is not None:
            params["nextToken"] = next_token
        s = requests.Session()
        resp = s.get(f"{cfg.api_url}/templates/{template_id}", params=params)
        _check_response(resp)
        return TemplateInfo.from_dict(resp.json())


    @classmethod
    def build(
        cls,
        *,
        name: str | None = None,
        image: str | None = None,
        dockerfile: str | None = None,
        start_cmd: str | None = None,
        cpu_count: int | None = None,
        memory_mb: int | None = None,
        envs: Dict[str, str] | None = None,
        config: Config | None = None,
        **kwargs: Any,
    ) -> TemplateInfo:
        """POST /v2/templates — Build (create) a new template.

        Submits a template build request and returns immediately with the
        template info (status will be ``"building"``).  Poll
        :meth:`get` to wait for the build to finish.

        Args:
            name: Human-readable template name / alias.
            image: Base container image URI (e.g. ``"python:3.11-slim"``).
            dockerfile: Inline Dockerfile content.  Mutually exclusive with
                *image*.
            start_cmd: Command to run when the sandbox starts.
            cpu_count: Number of vCPUs for the sandbox.
            memory_mb: Memory limit in MiB for the sandbox.
            envs: Environment variables baked into the template.
            config: SDK config.  Uses default (env-based) config if omitted.
            **kwargs: Extra fields forwarded verbatim to the request body.

        Returns:
            :class:`TemplateInfo` with the new ``template_id``.

        Raises:
            ApiError: On unexpected backend error.
        """
        cfg = config or Config()
        payload: dict = {}
        if name is not None:
            payload["name"] = name
        if image is not None:
            payload["image"] = image
        if dockerfile is not None:
            payload["dockerfile"] = dockerfile
        if start_cmd is not None:
            payload["startCmd"] = start_cmd
        if cpu_count is not None:
            payload["cpuCount"] = cpu_count
        if memory_mb is not None:
            payload["memoryMB"] = memory_mb
        if envs is not None:
            payload["envs"] = envs
        payload.update(kwargs)

        s = requests.Session()
        resp = s.post(
            f"{cfg.api_url}/v2/templates",
            json=payload,
            headers={"Content-Type": "application/json"},
        )
        _check_response(resp)
        return TemplateInfo.from_dict(resp.json())


    @classmethod
    def update(
        cls,
        template_id: str,
        *,
        name: str | None = None,
        public: bool | None = None,
        cpu_count: int | None = None,
        memory_mb: int | None = None,
        start_cmd: str | None = None,
        config: Config | None = None,
        **kwargs: Any,
    ) -> TemplateInfo:
        """PATCH /v2/templates/:templateID — Update template metadata.

        Only the fields you pass will be updated (PATCH semantics).

        Args:
            template_id: Template identifier.
            name: New template name.
            public: Whether the template is publicly accessible.
            cpu_count: New vCPU count.
            memory_mb: New memory limit in MiB.
            start_cmd: New sandbox start command.
            config: SDK config.  Uses default (env-based) config if omitted.
            **kwargs: Extra fields forwarded verbatim to the request body.

        Returns:
            Updated :class:`TemplateInfo`.

        Raises:
            TemplateNotFoundError: If the template does not exist (HTTP 404).
            ApiError: On unexpected backend error.
        """
        cfg = config or Config()
        payload: dict = {}
        if name is not None:
            payload["name"] = name
        if public is not None:
            payload["public"] = public
        if cpu_count is not None:
            payload["cpuCount"] = cpu_count
        if memory_mb is not None:
            payload["memoryMB"] = memory_mb
        if start_cmd is not None:
            payload["startCmd"] = start_cmd
        payload.update(kwargs)

        s = requests.Session()
        resp = s.patch(
            f"{cfg.api_url}/v2/templates/{template_id}",
            json=payload,
            headers={"Content-Type": "application/json"},
        )
        _check_response(resp)
        body = resp.json() if resp.content else {}
        return TemplateInfo.from_dict(body) if body else TemplateInfo(template_id=template_id)


    @classmethod
    def delete(cls, template_id: str, *, config: Config | None = None) -> None:
        """DELETE /templates/:templateID — Delete a template permanently.

        .. note::
            Snapshots are stored as templates.  You can also use
            :meth:`~cubesandbox.Sandbox.delete_snapshot` which calls this same
            endpoint but with slightly different error-handling semantics.

        Args:
            template_id: Template / snapshot identifier.
            config: SDK config.  Uses default (env-based) config if omitted.

        Raises:
            TemplateNotFoundError: If the template does not exist (HTTP 404).
            ApiError: On unexpected backend error.
        """
        cfg = config or Config()
        s = requests.Session()
        resp = s.delete(f"{cfg.api_url}/templates/{template_id}")
        _check_response(resp)

