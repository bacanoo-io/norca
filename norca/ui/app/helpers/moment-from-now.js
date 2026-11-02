/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import moment from 'moment';
import RelativeTimeHelper from 'nomad-ui/utils/relative-time-helper';

// {{moment-from-now date [hideAffix=true] [interval=ms]}}: "3 minutes ago"
export default class MomentFromNow extends RelativeTimeHelper {
  format([date], { hideAffix }) {
    return moment(date).fromNow(!!hideAffix);
  }
}
