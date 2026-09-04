# pyright: reportMissingImports=false

from pathlib import Path

import pytest

from codex_broker.client_auth import ClientKeyService
from codex_broker.database import Database
from codex_broker.errors import WindowkeeperError


@pytest.mark.asyncio
async def test_client_keys_are_hashed_authenticated_and_revoked(tmp_path: Path) -> None:
    database = Database(tmp_path / "broker.db")
    database.start()
    service = ClientKeyService(database)
    try:
        issued = await service.create("Pi")
        assert issued.token.startswith("cbk_")
        stored = await database.call(
            lambda connection: connection.execute("SELECT * FROM client_api_keys").fetchone()
        )
        assert issued.token.encode() not in bytes(stored["secret_hash"])
        assert (await service.authenticate(issued.token))["key_id"] == issued.key_id
        assert await service.revoke(issued.key_id)
        with pytest.raises(WindowkeeperError, match="Client authentication failed"):
            await service.authenticate(issued.token)
        assert await service.delete_revoked(issued.key_id)
        assert await service.list() == []
    finally:
        await database.close()
