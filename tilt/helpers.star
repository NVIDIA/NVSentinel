# Merge all values files to get effective configuration (later files override earlier ones)
def merge_dicts(base, override):
    """Recursively merge override into base, returning a new dict."""
    if not override:
        return base

    result = dict(base)
    for key, value in override.items():
        if key in result and type(result[key]) == "dict" and type(value) == "dict":
            result[key] = merge_dicts(result[key], value)
        else:
            result[key] = value
    return result
