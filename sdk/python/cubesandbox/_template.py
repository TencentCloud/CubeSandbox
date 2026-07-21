# Copyright (c) 2026 Tencent Inc.
# SPDX-License-Identifier: Apache-2.0

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Dict

import requests

from ._config import Config, _auth_headers
from ._exceptions import ApiError, AuthenticationError, TemplateNotFoundError
from ._policy import _validate_allow_out_domains_require_deny_all


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
    """A template create/rebuild job or build status record."""

    build_id: str
    status: str
    template_id: str = ""
    phase: str = ""
    progress: int = 0
    error_message: str = ""
    message: str = ""
    created_at: str = ""
    finished_at: str = ""
    logs: list[str] = field(default_factory=list)

    @classmethod
    def from_dict(cls, data: dict) -> "TemplateBuild":
        return cls(
            build_id=data.get("buildID") or data.get("jobID") or data.get("build_id", ""),
            template_id=data.get("templateID") or data.get("template_id", ""),
            status=data.get("status", ""),
            phase=data.get("phase", ""),
            progress=data.get("progress", 0),
            error_message=data.get("errorMessage") or data.get("error_message", ""),
            message=data.get("message", ""),
            created_at=data.get("createdAt") or data.get("created_at", ""),
            finished_at=data.get("finishedAt") or data.get("finished_at", ""),
            logs=data.get("logs") or [],
        )

    @property
    def job_id(self) -> str:
        """Alias for create/rebuild responses that use ``jobID``."""
        return self.build_id


@dataclass
class TemplateInfo:
    """Metadata for a CubeSandbox template."""

    template_id: str
    name: str = ""
    instance_type: str = ""
    version: str = ""
    status: str = ""
    last_error: str = ""
    created_at: str = ""
    image_info: str = ""
    job_id: str = ""
    public: bool = False
    cpu_count: int = 0
    memory_mb: int = 0
    replicas: list[dict] = field(default_factory=list)
    create_request: dict | None = None
    network_type: str | None = None
    allow_internet_access: bool | None = None
    builds: list[TemplateBuild] = field(default_factory=list)

    @classmethod
    def from_dict(cls, data: dict) -> "TemplateInfo":
        builds_raw = data.get("builds") or []
        aliases = data.get("aliases") or []
        return cls(
            template_id=data.get("templateID") or data.get("template_id", ""),
            name=data.get("name") or (aliases[0] if aliases else "") or "",
            instance_type=data.get("instanceType") or data.get("instance_type", ""),
            version=data.get("version") or "",
            status=data.get("status") or "",
            last_error=data.get("lastError") or data.get("last_error", ""),
            created_at=data.get("createdAt") or data.get("created_at", ""),
            image_info=data.get("imageInfo") or data.get("image_info", ""),
            job_id=data.get("jobID") or data.get("job_id", ""),
            public=bool(data.get("public", False)),
            cpu_count=data.get("cpuCount") or data.get("cpu_count", 0),
            memory_mb=data.get("memoryMB") or data.get("memory_mb", 0),
            replicas=data.get("replicas") or [],
            create_request=data.get("createRequest") or data.get("create_request"),
            network_type=data.get("networkType") or data.get("network_type"),
            allow_internet_access=(
                data.get("allowInternetAccess")
                if "allowInternetAccess" in data
                else data.get("allow_internet_access")
            ),
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

        # Build a new template from a container image (template ID is auto-generated)
        job = Template.build(
            image="python:3.11-slim",
        )
        print(job.job_id, job.status)

        # Query a specific template
        detail = Template.get("tpl-xxxxxxxxxxxxxxxxxxxxxxxx")
        print(detail.status, detail.image_info)

        # Rebuild an existing template
        Template.rebuild("tpl-xxx")

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
        resp = s.get(f"{cfg.api_url}/templates", headers=_auth_headers(cfg))
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
        resp = s.get(f"{cfg.api_url}/templates/{template_id}", params=params,
                     headers=_auth_headers(cfg))
        _check_response(resp)
        return TemplateInfo.from_dict(resp.json())


    @classmethod
    def build(
        cls,
        *,
        template_id: str | None = None,  # Deprecated: server always auto-generates template IDs with "tpl-" prefix.
        name: str | None = None,  # E2B template name → forwarded as stable alias.
        image: str | None = None,
        dockerfile: str | None = None,
        start_cmd: str | None = None,
        instance_type: str | None = None,
        writable_layer_size: str | None = None,
        exposed_ports: list[int] | None = None,
        probe_port: int | None = None,
        probe_path: str | None = None,
        cpu_count: int | None = None,
        memory_mb: int | None = None,
        envs: Dict[str, str] | None = None,
        allow_internet_access: bool | None = None,
        network_type: str | None = None,
        nodes: list[str] | None = None,
        registry_username: str | None = None,
        registry_password: str | None = None,
        command: list[str] | None = None,
        args: list[str] | None = None,
        dns: list[str] | None = None,
        allow_out: list[str] | None = None,
        deny_out: list[str] | None = None,
        enable_ivshmem: bool | None = None,
        config: Config | None = None,
        **kwargs: Any,
    ) -> TemplateBuild:
        """POST /templates - Build (create) a new template from an image.

        Submits CubeAPI's create-from-image request and returns the async job
        accepted by CubeMaster. Poll :meth:`get_build_status` or :meth:`get`
        to watch the build finish.

        Args:
            template_id: Template ID. Deprecated: the server always auto-generates template IDs
                with the "tpl-" prefix. This parameter is accepted for backward compatibility
                but its value is ignored.
            name: E2B-compatible template name. Forwarded as ``"name"`` in the request body;
                CubeAPI derives a stable alias from it so sandboxes can reference the template
                by this name instead of the auto-generated ``tpl-*`` ID.
            image: Base container image URI (e.g. ``"python:3.11-slim"``).
            dockerfile: Not supported by CubeAPI's current template endpoint.
            start_cmd: Not supported by CubeAPI's current template endpoint.
            instance_type: CubeMaster instance type (defaults server-side).
            writable_layer_size: Writable layer size, e.g. ``"1G"``.
            exposed_ports: Container ports to expose.
            probe_port: HTTP probe port.
            probe_path: HTTP probe path.
            cpu_count: CPU in millicores. Sent as CubeAPI ``cpu``.
            memory_mb: Memory limit in MiB for the sandbox.
            envs: Environment variables baked into the template.
            allow_internet_access: Whether sandboxes from this template may access internet.
            network_type: Network mode for the generated template, e.g. ``"tap"``.
            nodes: Limit template distribution to these node IDs or host IPs.
            registry_username: Registry username for private source images.
            registry_password: Registry password for private source images.
            command: Override container ENTRYPOINT.
            args: Override container CMD args.
            dns: Container DNS nameservers.
            allow_out: Allowed outbound CIDRs for CubeVS egress policy.
            deny_out: Denied outbound CIDRs for CubeVS egress policy.
            enable_ivshmem: Whether the template build sandbox should boot with ivshmem enabled.
            config: SDK config.  Uses default (env-based) config if omitted.
            **kwargs: Extra fields forwarded verbatim to the request body.

        Returns:
            :class:`TemplateBuild` with ``job_id``, ``template_id`` and status fields.

        Raises:
            ValueError: If ``image`` is missing or unsupported fields are used.
            ApiError: On unexpected backend error.
        """
        if dockerfile is not None:
            raise ValueError("dockerfile builds are not supported by CubeAPI /templates")
        if start_cmd is not None:
            raise ValueError("start_cmd is not supported by CubeAPI /templates")
        if not image or not image.strip():
            raise ValueError("image is required")
        _validate_allow_out_domains_require_deny_all(
            allow_out,
            deny_out,
            default_deny_all=allow_internet_access is False,
        )

        cfg = config or Config()
        payload: dict = {"image": image.strip()}
        if name is not None:
            payload["name"] = name
        if instance_type is not None:
            payload["instanceType"] = instance_type
        if writable_layer_size is not None:
            payload["writableLayerSize"] = writable_layer_size
        if exposed_ports is not None:
            payload["exposedPorts"] = exposed_ports
        if probe_port is not None:
            payload["probePort"] = probe_port
        if probe_path is not None:
            payload["probePath"] = probe_path
        if cpu_count is not None:
            payload["cpu"] = cpu_count
        if memory_mb is not None:
            payload["memory"] = memory_mb
        if envs is not None:
            payload["env"] = [f"{key}={value}" for key, value in envs.items()]
        if allow_internet_access is not None:
            payload["allowInternetAccess"] = allow_internet_access
        if network_type is not None:
            payload["networkType"] = network_type
        if nodes is not None:
            payload["nodes"] = nodes
        if registry_username is not None:
            payload["registryUsername"] = registry_username
        if registry_password is not None:
            payload["registryPassword"] = registry_password
        if command is not None:
            payload["command"] = command
        if args is not None:
            payload["args"] = args
        if dns is not None:
            payload["dns"] = dns
        if allow_out is not None:
            payload["allowOut"] = allow_out
        if deny_out is not None:
            payload["denyOut"] = deny_out
        if enable_ivshmem is not None:
            payload["enableIvshmem"] = enable_ivshmem
        payload.update(kwargs)

        s = requests.Session()
        resp = s.post(
            f"{cfg.api_url}/templates",
            json=payload,
            headers={"Content-Type": "application/json", **_auth_headers(cfg)},
        )
        _check_response(resp)
        return TemplateBuild.from_dict(resp.json())


    @classmethod
    def rebuild(
        cls,
        template_id: str,
        *,
        config: Config | None = None,
        **extra: Any,
    ) -> TemplateBuild:
        """POST /templates/:templateID - Rebuild an existing template."""
        cfg = config or Config()
        s = requests.Session()
        resp = s.post(
            f"{cfg.api_url}/templates/{template_id}",
            json=extra,
            headers={"Content-Type": "application/json", **_auth_headers(cfg)},
        )
        _check_response(resp)
        return TemplateBuild.from_dict(resp.json())


    @classmethod
    def get_build_status(
        cls,
        template_id: str,
        build_id: str,
        *,
        config: Config | None = None,
    ) -> TemplateBuild:
        """GET /templates/:templateID/builds/:buildID/status."""
        cfg = config or Config()
        s = requests.Session()
        resp = s.get(f"{cfg.api_url}/templates/{template_id}/builds/{build_id}/status",
                     headers=_auth_headers(cfg))
        _check_response(resp)
        return TemplateBuild.from_dict(resp.json())


    @classmethod
    def get_build_logs(
        cls,
        template_id: str,
        build_id: str,
        *,
        config: Config | None = None,
    ) -> dict:
        """GET /templates/:templateID/builds/:buildID/logs."""
        cfg = config or Config()
        s = requests.Session()
        resp = s.get(f"{cfg.api_url}/templates/{template_id}/builds/{build_id}/logs",
                     headers=_auth_headers(cfg))
        _check_response(resp)
        return resp.json()


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
        """Template metadata updates are not supported by current CubeAPI.

        CubeAPI exposes ``PATCH /templates/:templateID`` but the handler
        returns NotImplemented. Use :meth:`rebuild` to rebuild a template or
        delete and recreate it.
        """
        raise NotImplementedError(
            "CubeAPI does not support template metadata updates; use Template.rebuild() "
            "or delete and recreate the template"
        )


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
        resp = s.delete(f"{cfg.api_url}/templates/{template_id}", headers=_auth_headers(cfg))
        _check_response(resp)
