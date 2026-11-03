docs: add Integrating an External Health Detector tutorial

This PR adds a generic tutorial page for external health detectors in NVSentinel, using NPD and Dual-Key as worked examples, and updates the existing NPD page with a link.

### Summary of Changes:
1. **New Tutorial Page (`integrating-external-health-detectors.md`):**
   - Outlines the integration pattern: external tool sets a Node Condition -> NVSentinel's Kubernetes Object Monitor (KOM) turns it into a health event -> NVSentinel cordons and drains.
   - Sets clear rules for detectors (single owner, `True` means problem, clearing only after verified recovery, distinct policy names).
   - Provides policy configuration guidelines for node-level vs. GPU-level components.
2. **Worked Examples:**
   - **NPD (Node Problem Detector):** Links to the existing host-log pattern.
   - **Dual-Key:** Adds a GPU inference-integrity checker example detailing how silent corruption is detected and cleared.
3. **Documentation Structure:**
   - Leaves the original NPD tutorial intact, adding a reference link to the new external detector page for cleaner review.
