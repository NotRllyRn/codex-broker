from pathlib import Path


def test_dockerfile_preserves_wheel_filename() -> None:
    dockerfile = (Path(__file__).parents[2] / "Dockerfile").read_text(encoding="utf-8")

    assert "COPY --from=build /build/dist/*.whl /tmp/" in dockerfile
    assert "pip install --no-cache-dir /tmp/*.whl" in dockerfile
    assert 'ENTRYPOINT ["python","-m","codex_broker.container_entrypoint"]' in dockerfile


def test_public_enrollment_compose_has_no_broker_state_mount() -> None:
    compose = (Path(__file__).parents[2] / "compose.yaml").read_text(encoding="utf-8")
    public = compose.split("  public-enrollment:", 1)[1].split("\nvolumes:", 1)[0]

    assert 'command: ["public-serve"]' in public
    assert "/data" not in public
    assert "server.key" not in public
    assert "ca.key" not in public
    assert "read_only: true" in public
    assert "no-new-privileges:true" in public
