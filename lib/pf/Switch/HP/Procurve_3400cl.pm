package pf::Switch::HP::Procurve_3400cl;

=head1 NAME

pf::Switch::HP::Procurve_3400cl - Object oriented module to parse SNMP traps and manage HP Procurve 3400cl switches

=head1 STATUS

This switch was reported to work by the community with the Procurve 2600 module.

=head1 SNMP

This switch can parse SNMP trap and change a Vlan on a switch port with SNMP.

=cut

use strict;
use warnings;
use Net::SNMP;
use base ('pf::Switch::HP');

sub description { 'HP ProCurve 3400cl Series' }

=head1 AUTHOR

Inverse inc. <info@inverse.ca>

=head1 COPYRIGHT

Copyright (C) 2005-2021 Inverse inc.

=head1 LICENSE

This program is free software; you can redistribute it and/or
modify it under the terms of the GNU General Public License
as published by the Free Software Foundation; either version 2
of the License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program; if not, write to the Free Software
Foundation, Inc., 51 Franklin Street, Fifth Floor, Boston, MA  02110-1301,
USA.

=cut

1;

# vim: set shiftwidth=4:
# vim: set expandtab:
# vim: set backspace=indent,eol,start:
