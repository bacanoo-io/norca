/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// Formatting of the values of the metrics store by unit, for the axes, the
// tooltips and the textual summaries of the history charts.

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];

function round(value, digits = 1) {
  const f = 10 ** digits;
  return Math.round(value * f) / f;
}

export function formatBytesValue(bytes) {
  let value = Math.abs(bytes) || 0;
  let unit = 0;
  while (value >= 1024 && unit < BYTE_UNITS.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${round(Math.sign(bytes || 0) * value)} ${BYTE_UNITS[unit]}`;
}

export function formatSeconds(seconds) {
  if (seconds == null) return '–';
  if (seconds !== 0 && Math.abs(seconds) < 1) {
    return `${round(seconds * 1000)} ms`;
  }
  if (Math.abs(seconds) < 120) return `${round(seconds)} s`;
  if (Math.abs(seconds) < 7200) return `${round(seconds / 60)} min`;
  return `${round(seconds / 3600)} h`;
}

/**
 * formatMetric formats a value of the unit: percent, ratio (0 to 1, shown in
 * percent), mib (MiB), bytes, seconds, state (1 is true) or count (the
 * default).
 */
export function formatMetric(value, unit) {
  if (value == null || Number.isNaN(value)) return '–';
  switch (unit) {
    case 'percent':
      return `${round(value)}%`;
    case 'mib':
      return formatBytesValue(value * 1024 * 1024);
    case 'bytes':
      return formatBytesValue(value);
    case 'seconds':
      return formatSeconds(value);
    case 'ratio':
      return `${round(value * 100)}%`;
    case 'state':
      return value >= 0.5 ? 'yes' : 'no';
    default:
      return `${round(value, 2)}`;
  }
}

/**
 * formatDuration formats a duration in nanoseconds (as the API encodes a Go
 * duration) in days and hours: 720h is "30d", 36h "1d 12h".
 */
export function formatDuration(ns) {
  if (!(ns > 0)) return '–';
  const hours = ns / 3600e9;
  if (hours < 1) return `${Math.round(ns / 60e9)}m`;
  const days = Math.floor(hours / 24);
  const rest = Math.round(hours - days * 24);
  if (!days) return `${rest}h`;
  return rest ? `${days}d ${rest}h` : `${days}d`;
}
