DROP TABLE IF EXISTS sbom;
DROP TABLE IF EXISTS vuln;

CREATE TABLE sbom (
    image TEXT,
    name TEXT,
    type TEXT
);

CREATE TABLE vuln (
    image TEXT,
    cve TEXT,
    pkg TEXT,
    installed TEXT,
    severity TEXT
);

.mode csv
.headers off
.separator ","
.import sbom.csv sbom
.import vuln.csv vuln