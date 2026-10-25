/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import moment from 'moment';
import RelativeTimeHelper from 'nomad-ui/utils/relative-time-helper';

// {{moment-to-now date [hideAffix=true] [interval=ms]}}: "in 3 minutes"
export default class MomentToNow extends RelativeTimeHelper {
  format([date], { hideAffix }) {
    return moment(date).toNow(!!hideAffix);
  }
}
