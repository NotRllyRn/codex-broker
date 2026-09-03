from codex_broker.web.app import _templates


def test_all_templates_compile() -> None:
    environment = _templates()
    for name in environment.list_templates():
        environment.get_template(name)
