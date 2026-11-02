/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';

/**
 * The task drivers of a client: one row per driver with its health, and
 * the details (health description and attributes) of a detected driver
 * when expanded.
 *
 *   <Clients::DriverList @drivers={{this.sortedDrivers}} />
 */
export default class ClientsDriverList extends Component {
  @tracked expanded = [];

  isExpanded = (driver) => this.expanded.includes(driver.name);

  @action
  toggle(driver) {
    this.expanded = this.isExpanded(driver)
      ? this.expanded.filter((name) => name !== driver.name)
      : [...this.expanded, driver.name];
  }
}
