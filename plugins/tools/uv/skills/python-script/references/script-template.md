# Python script template

Use this template for new automation scripts.

```python
#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.12"
# dependencies = [
#     "click",
#     "rich",
#     "requests",
# ]
# ///

import logging
from pathlib import Path

import click
from rich.console import Console
from rich.logging import RichHandler

LOG_DIR = Path(".agents/logs")
LOG_DIR.mkdir(parents=True, exist_ok=True)

logging.basicConfig(
    level=logging.INFO,
    format="%(message)s",
    datefmt="[%X]",
    handlers=[
        RichHandler(console=Console(stderr=True)),
        logging.FileHandler(LOG_DIR / "example-script.log"),
    ],
)
logger = logging.getLogger(__name__)


@click.command()
@click.option("--dry-run", is_flag=True, help="Preview without making changes")
def main(dry_run: bool) -> None:
    logger.info("Starting script execution")
    if dry_run:
        logger.info("[DRY RUN] Would perform action here")
        return
    logger.info("Script completed successfully")


if __name__ == "__main__":
    main()
```
