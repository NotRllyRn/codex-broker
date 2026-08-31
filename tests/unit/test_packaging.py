from pathlib import Path


def test_dockerfile_preserves_wheel_filename() -> None:
    dockerfile = (Path(__file__).parents[2] / "Dockerfile").read_text(encoding="utf-8")

    assert "COPY --from=build /build/dist/*.whl /tmp/" in dockerfile
    assert "pip install --no-cache-dir /tmp/*.whl" in dockerfile
    assert 'ENTRYPOINT ["python","-m","windowkeeper.container_entrypoint"]' in dockerfile
