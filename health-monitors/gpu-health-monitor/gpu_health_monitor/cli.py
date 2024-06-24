import click


@click.command()
@click.option("--socket", "-s", default="/var/run/nvsentinel.sock", help="unix domain socket path")
def cli(socket):
    print("Hello, World")


if __name__ == "__main__":
    cli()
