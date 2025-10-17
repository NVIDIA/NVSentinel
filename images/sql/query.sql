-- 1. Unique vulns per image
SELECT image, COUNT(DISTINCT cve) AS unique_vulnerabilities
FROM vuln
GROUP BY image
ORDER BY unique_vulnerabilities DESC;

-- 2. Unique vulns per image per severity
SELECT image, severity, COUNT(DISTINCT cve) AS unique_vulnerabilities
FROM vuln
GROUP BY image, severity
ORDER BY image, severity;

-- 3. Unique packages per image per type
SELECT image, type, COUNT(DISTINCT name) AS unique_packages
FROM sbom
GROUP BY image, type
ORDER BY image, type;