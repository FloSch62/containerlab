document.addEventListener("DOMContentLoaded", function () {
  var blocks = document.querySelectorAll(".md-typeset pre");
  blocks.forEach(function (pre) {
    var code = pre.querySelector("code");
    if (!code || code.scrollWidth <= pre.clientWidth) return;
    var container = pre.parentElement;
    var copyBtn = container.querySelector(
      '.md-code__button[data-md-type="copy"]'
    );

    var btn = document.createElement("button");
    btn.className = "md-code__button md-icon";
    btn.type = "button";
    btn.dataset.mdType = "expand";
    btn.title = "Expand code";

    if (copyBtn) {
      copyBtn.after(btn);
    } else {
      container.insertBefore(btn, pre);
    }

    var tooltip = document.createElement("div");
    tooltip.className = "md-tooltip2 md-tooltip2--bottom";
    tooltip.setAttribute("role", "tooltip");
    var inner = document.createElement("div");
    inner.className = "md-tooltip2__inner";
    inner.textContent = btn.title;
    tooltip.appendChild(inner);
    document.body.appendChild(tooltip);

    function positionTooltip() {
      var rect = btn.getBoundingClientRect();
      tooltip.style.setProperty(
        "--md-tooltip-host-x",
        window.scrollX + rect.left + rect.width / 2 + "px"
      );
      tooltip.style.setProperty(
        "--md-tooltip-host-y",
        window.scrollY + rect.top + "px"
      );
      tooltip.style.setProperty("--md-tooltip-x", "0px");
      tooltip.style.setProperty(
        "--md-tooltip-y",
        8 + rect.height + "px"
      );
      var w = inner.offsetWidth;
      tooltip.style.setProperty("--md-tooltip-width", w + "px");
      tooltip.style.setProperty("--md-tooltip-tail", "0px");
    }

    function showTooltip() {
      positionTooltip();
      tooltip.classList.add("md-tooltip2--active");
    }

    function hideTooltip() {
      tooltip.classList.remove("md-tooltip2--active");
    }

    btn.addEventListener("mouseenter", showTooltip);
    btn.addEventListener("focus", showTooltip);
    btn.addEventListener("mouseleave", hideTooltip);
    btn.addEventListener("blur", hideTooltip);

    btn.addEventListener("click", function () {
      var overlay = document.querySelector(".code-overlay");
      if (!overlay) {
        overlay = document.createElement("div");
        overlay.className = "md-typeset code-overlay";
        var wrapper = document.createElement("div");
        wrapper.className = container.className;
        var clone = pre.cloneNode(true);
        wrapper.appendChild(clone);
        overlay.appendChild(wrapper);
        document.body.appendChild(overlay);
        overlay.addEventListener("click", function () {
          overlay.remove();
          btn.dataset.mdType = "expand";
          var label = "Expand code";
          btn.title = label;
          inner.textContent = label;
          showTooltip();
        });
        btn.dataset.mdType = "collapse";
        var label = "Collapse code";
        btn.title = label;
        inner.textContent = label;
        showTooltip();
      } else {
        overlay.remove();
        btn.dataset.mdType = "expand";
        var label = "Expand code";
        btn.title = label;
        inner.textContent = label;
        showTooltip();
      }
    });
  });

  var tables = document.querySelectorAll(".md-typeset table");
  tables.forEach(function (table) {
    var headerCell = table.querySelector("thead th:last-child");
    if (!headerCell) {
      headerCell = table.querySelector("tr:first-child th:last-child");
    }
    if (!headerCell) return;

    var btn = document.createElement("button");
    btn.className = "md-table__button md-icon";
    btn.type = "button";
    btn.dataset.mdType = "expand";
    btn.title = "Expand table";
    headerCell.appendChild(btn);

    var tooltip = document.createElement("div");
    tooltip.className = "md-tooltip2 md-tooltip2--bottom";
    tooltip.setAttribute("role", "tooltip");
    var inner = document.createElement("div");
    inner.className = "md-tooltip2__inner";
    inner.textContent = btn.title;
    tooltip.appendChild(inner);
    document.body.appendChild(tooltip);

    function positionTooltip() {
      var rect = btn.getBoundingClientRect();
      tooltip.style.setProperty(
        "--md-tooltip-host-x",
        window.scrollX + rect.left + rect.width / 2 + "px"
      );
      tooltip.style.setProperty(
        "--md-tooltip-host-y",
        window.scrollY + rect.top + "px"
      );
      tooltip.style.setProperty("--md-tooltip-x", "0px");
      tooltip.style.setProperty("--md-tooltip-y", 8 + rect.height + "px");
      var w = inner.offsetWidth;
      tooltip.style.setProperty("--md-tooltip-width", w + "px");
      tooltip.style.setProperty("--md-tooltip-tail", "0px");
    }

    function showTooltip() {
      positionTooltip();
      tooltip.classList.add("md-tooltip2--active");
    }

    function hideTooltip() {
      tooltip.classList.remove("md-tooltip2--active");
    }

    btn.addEventListener("mouseenter", showTooltip);
    btn.addEventListener("focus", showTooltip);
    btn.addEventListener("mouseleave", hideTooltip);
    btn.addEventListener("blur", hideTooltip);

    btn.addEventListener("click", function () {
      var overlay = document.querySelector(".code-overlay");
      if (!overlay) {
        overlay = document.createElement("div");
        overlay.className = "md-typeset code-overlay";
        var wrapper = document.createElement("div");
        wrapper.className = table.parentElement.className;
        var clone = table.cloneNode(true);
        wrapper.appendChild(clone);
        overlay.appendChild(wrapper);
        document.body.appendChild(overlay);
        overlay.addEventListener("click", function () {
          overlay.remove();
          btn.dataset.mdType = "expand";
          var label = "Expand table";
          btn.title = label;
          inner.textContent = label;
          showTooltip();
        });
        btn.dataset.mdType = "collapse";
        var label = "Collapse table";
        btn.title = label;
        inner.textContent = label;
        showTooltip();
      } else {
        overlay.remove();
        btn.dataset.mdType = "expand";
        var label = "Expand table";
        btn.title = label;
        inner.textContent = label;
        showTooltip();
      }
    });
  });
});
