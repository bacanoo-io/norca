/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import moment from 'moment';
import RelativeTimeHelper from 'nomad-ui/utils/relative-time-helper';

// {{moment-from date [reference] [hideAffix=true] [interval=ms]}}: the time
// of `date` relative to `reference` (now when omitted).
export default class MomentFrom extends RelativeTimeHelper {
  format([date, reference], { hideAffix }) {
    return moment(date).from(
      reference ? moment(reference) : moment(),
      !!hideAffix
    );
  }
}
