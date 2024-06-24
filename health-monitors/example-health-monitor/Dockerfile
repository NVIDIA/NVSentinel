FROM python:3.10-bookworm as build

RUN pip install poetry==1.8.2

ENV POETRY_NO_INTERACTION=1 \
    POETRY_VIRTUALENVS_IN_PROJECT=1 \
    POETRY_VIRTUALENVS_CREATE=1 \
    POETRY_CACHE_DIR=/tmp/poetry_cache

WORKDIR /app

COPY . .
RUN poetry build --format wheel 
RUN poetry export --format requirements.txt --output constraints.txt --without-hashes

FROM python:3.10.14-slim-bookworm as runtime

ENV PYTHONUNBUFFERED=1

COPY --from=build /app/dist/*.whl ./
COPY --from=build /app/constraints.txt ./
RUN pip install ./example_health_monitor*.whl --constraint constraints.txt

ENTRYPOINT ["examplehealthmonitor"]
